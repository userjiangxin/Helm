// Package supervisor owns the Runs on this machine: it claims work, prepares
// workdirs, starts executors, applies directives, numbers events and enforces policy.
package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/helm/helm/daemon/internal/executor"
	"github.com/helm/helm/daemon/internal/policy"
	"github.com/helm/helm/daemon/internal/wire"
)

// Control is the subset of conn.Client the supervisor needs (an interface for tests).
type Control interface {
	Claim(ctx context.Context, slots int) ([]wire.RunAssignment, error)
	Directives(ctx context.Context, after string) ([]wire.Directive, error)
	Ack(ctx context.Context, a wire.DirectiveAck) error
	Publish(ctx context.Context, b wire.RunEvents)
}

type run struct {
	a       wire.RunAssignment
	state   wire.RunState
	engine  *policy.Engine
	exec    executor.Executor
	session executor.SessionID
	seq     int64
	// turns counts user messages the agent has not finished answering. A claim only moves the
	// run forward when it answers the latest message (turns reaches 0); otherwise a late result
	// from an earlier turn could mark the run VERIFYING while newer instructions are pending.
	turns int
}

type Supervisor struct {
	ctl       Control
	executors map[string]executor.Executor
	maxRuns   int
	workRoot  string
	hookSock  string
	log       *slog.Logger

	wakeMu sync.Mutex // serializes OnWake so claims never race

	mu       sync.Mutex
	runs     map[string]*run
	lastDirs string
}

type Options struct {
	Executors []executor.Executor
	MaxRuns   int
	WorkRoot  string // e.g. ~/helm-work; worktrees live under it
	HookSock  string
	Log       *slog.Logger
}

func New(ctl Control, o Options) *Supervisor {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	s := &Supervisor{ctl: ctl, executors: map[string]executor.Executor{}, maxRuns: o.MaxRuns,
		workRoot: o.WorkRoot, hookSock: o.HookSock, log: o.Log, runs: map[string]*run{}}
	for _, e := range o.Executors {
		s.executors[e.Name()] = e
		if ch := e.Events(); ch != nil {
			go s.pump(ch)
		}
	}
	return s
}

// pump turns executor output into L1 events and state transitions. Sessions are keyed by run id.
func (s *Supervisor) pump(ch <-chan executor.SessionEvent) {
	ctx := context.Background()
	for se := range ch {
		s.mu.Lock()
		r, ok := s.runs[string(se.Session)]
		s.mu.Unlock()
		if !ok {
			continue
		}
		if se.Done {
			s.mu.Lock()
			st := r.state
			s.mu.Unlock()
			switch {
			case isTerminal(st):
			case se.Err != nil:
				s.transition(ctx, r, wire.RunFailed, "agent process exited: "+se.Err.Error())
				s.forget(r.a.RunID)
			case st == wire.RunVerifying:
				// Process ended after reporting done (e.g. stdin closed); keep VERIFYING for the reviewer.
			default:
				s.transition(ctx, r, wire.RunFailed, "agent process exited unexpectedly")
				s.forget(r.a.RunID)
			}
			continue
		}
		s.emit(ctx, r, se.Event.Type, se.Event.Data)
		if se.Event.Type == wire.EvClaim {
			s.mu.Lock()
			st := r.state
			if r.turns > 0 {
				r.turns--
			}
			latest := r.turns == 0
			s.mu.Unlock()
			if c, ok := se.Event.Data.(wire.Claim); ok && st == wire.RunRunning && latest {
				if c.Kind == "done" {
					s.transition(ctx, r, wire.RunVerifying, "agent reports the task is done; awaiting verification")
				} else {
					s.transition(ctx, r, wire.RunGated, "agent turn ended with an error")
				}
			}
		}
	}
}

func isTerminal(st wire.RunState) bool {
	return st == wire.RunSucceeded || st == wire.RunFailed || st == wire.RunAborted
}

// ---------- conn.Handler ----------

func (s *Supervisor) Heartbeat() wire.Heartbeat {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.runs))
	for id := range s.runs {
		ids = append(ids, id)
	}
	// TODO(M0-D6): fill Load from gopsutil / nvidia-smi sampling.
	return wire.Heartbeat{RunningRuns: ids}
}

func (s *Supervisor) OnWake(ctx context.Context, reason string) {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	s.pullDirectives(ctx)
	if free := s.maxRuns - s.active(); free > 0 {
		as, err := s.ctl.Claim(ctx, free)
		if err != nil {
			s.log.Warn("claim failed", "err", err, "reason", reason)
			return
		}
		for _, a := range as {
			s.start(ctx, a)
		}
	}
}

func (s *Supervisor) active() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}

// ---------- hook.Decider ----------

func (s *Supervisor) Decide(runID string, req policy.Request) policy.Decision {
	s.mu.Lock()
	r, ok := s.runs[runID]
	s.mu.Unlock()
	if !ok {
		return policy.Decision{Action: policy.Deny, Reason: "unknown run " + runID}
	}
	d := r.engine.Decide(req)
	s.emit(context.Background(), r, wire.EvToolCall, wire.ToolCall{
		Tool: req.Tool, InputDigest: digest(req), Decision: string(d.Action), Reason: d.Reason})
	if d.Action == policy.Hold {
		s.transition(context.Background(), r, wire.RunGated, d.Reason)
	}
	return d
}

// ---------- run lifecycle ----------

func (s *Supervisor) start(ctx context.Context, a wire.RunAssignment) {
	ex, ok := s.executors[a.Executor]
	r := &run{a: a, state: wire.RunPending}
	s.mu.Lock()
	s.runs[a.RunID] = r
	s.mu.Unlock()
	if !ok {
		s.transition(ctx, r, wire.RunFailed, "executor not available on this machine: "+a.Executor)
		s.forget(a.RunID)
		return
	}
	r.exec = ex
	s.transition(ctx, r, wire.RunPreparing, "claimed")

	// TODO(M0-D2): prepare an isolated git worktree under s.workRoot (shared bare clone
	// cache per repo, like Multica's .repos), snapshot commit for ROLLBACK, link skills.
	workdir := a.Contract.Workdir
	r.engine = policy.New(workdir, a.Contract.Scope)

	sess, err := ex.Start(ctx, executor.RunSpec{RunID: a.RunID, Workdir: workdir, Contract: a.Contract, HookSock: s.hookSock})
	if err != nil {
		s.transition(ctx, r, wire.RunFailed, err.Error())
		s.forget(a.RunID)
		return
	}
	s.mu.Lock()
	r.session = sess
	if a.Contract.Prompt != "" {
		r.turns = 1 // the executor sends the contract as the first message
	}
	s.mu.Unlock()
	s.transition(ctx, r, wire.RunRunning, "executor started")
}

func (s *Supervisor) forget(runID string) {
	s.mu.Lock()
	delete(s.runs, runID)
	s.mu.Unlock()
}

func (s *Supervisor) transition(ctx context.Context, r *run, to wire.RunState, reason string) {
	s.mu.Lock()
	from := r.state
	r.state = to
	s.mu.Unlock()
	if from == to {
		return
	}
	s.emit(ctx, r, wire.EvStateChange, wire.StateChange{From: from, To: to, Reason: reason})
}

func (s *Supervisor) emit(ctx context.Context, r *run, t wire.EventType, data any) {
	s.mu.Lock()
	r.seq++
	ev := wire.Event{Seq: r.seq, TS: time.Now().UTC(), Type: t, Data: wire.MustData(data)}
	s.mu.Unlock()
	s.ctl.Publish(ctx, wire.RunEvents{RunID: r.a.RunID, Events: []wire.Event{ev}})
}

// ---------- directives ----------

func (s *Supervisor) pullDirectives(ctx context.Context) {
	s.mu.Lock()
	after := s.lastDirs
	s.mu.Unlock()
	ds, err := s.ctl.Directives(ctx, after)
	if err != nil {
		s.log.Warn("fetch directives failed", "err", err)
		return
	}
	for _, d := range ds {
		status, detail := s.apply(ctx, d)
		if err := s.ctl.Ack(ctx, wire.DirectiveAck{DirectiveID: d.ID, Status: status, Detail: detail, AckedAt: time.Now().UTC()}); err != nil {
			s.log.Warn("ack failed", "directive", d.ID, "err", err)
			return // retry from this directive next wake
		}
		s.mu.Lock()
		s.lastDirs = d.ID
		s.mu.Unlock()
	}
}

func (s *Supervisor) apply(ctx context.Context, d wire.Directive) (wire.AckStatus, string) {
	s.mu.Lock()
	r, ok := s.runs[d.RunID]
	s.mu.Unlock()
	if !ok {
		return wire.AckRejectedInvalid, "run not on this machine"
	}
	if d.ContractVersion < r.a.Contract.Version {
		return wire.AckRejectedStale, fmt.Sprintf("directive for v%d, run is at v%d", d.ContractVersion, r.a.Contract.Version)
	}
	var args struct {
		Message string      `json:"message"`
		Scope   *wire.Scope `json:"scope"` // full replacement scope for AMEND
	}
	if len(d.Args) > 0 {
		if err := json.Unmarshal(d.Args, &args); err != nil {
			return wire.AckRejectedInvalid, "bad args: " + err.Error()
		}
	}
	switch d.Kind {
	case wire.DirPause:
		r.engine.SetPaused(true)
		s.transition(ctx, r, wire.RunSuspended, "PAUSE")
	case wire.DirContinue:
		r.engine.SetPaused(false)
		s.transition(ctx, r, wire.RunRunning, "CONTINUE")
		msg := args.Message
		if msg == "" {
			msg = "Helm: approved. Continue with the task."
		}
		if err := s.send(ctx, r, msg); err != nil {
			return wire.AckFailed, err.Error()
		}
	case wire.DirAmend:
		if args.Scope == nil {
			return wire.AckRejectedInvalid, "AMEND needs args.scope"
		}
		r.engine.Apply(*args.Scope)
		r.a.Contract.Version = d.ContractVersion
		r.a.Contract.Scope = *args.Scope
		if args.Message != "" {
			if err := s.send(ctx, r, args.Message); err != nil {
				return wire.AckFailed, "scope updated but message not delivered: " + err.Error()
			}
		}
	case wire.DirSteer:
		if args.Message == "" {
			return wire.AckRejectedInvalid, "STEER needs args.message"
		}
		if err := s.send(ctx, r, args.Message); err != nil {
			return wire.AckFailed, err.Error()
		}
		s.mu.Lock()
		st := r.state
		s.mu.Unlock()
		if st == wire.RunVerifying || st == wire.RunGated {
			s.transition(ctx, r, wire.RunRunning, "STEER")
		}
	case wire.DirComplete:
		s.mu.Lock()
		st := r.state
		s.mu.Unlock()
		if st != wire.RunVerifying {
			return wire.AckRejectedInvalid, "COMPLETE requires VERIFYING, run is " + string(st)
		}
		s.transition(ctx, r, wire.RunSucceeded, "accepted by reviewer")
		_ = r.exec.Stop(ctx, r.session, 10*time.Second)
		s.forget(r.a.RunID)
	case wire.DirAbort:
		s.transition(ctx, r, wire.RunAborted, "ABORT") // first, so the exit is not reported as a failure
		_ = r.exec.Stop(ctx, r.session, 10*time.Second)
		s.forget(r.a.RunID)
	case wire.DirRollback:
		return wire.AckFailed, "ROLLBACK not implemented yet (M1)"
	default:
		return wire.AckRejectedInvalid, "unknown directive " + string(d.Kind)
	}
	return wire.AckApplied, ""
}

// send delivers a user message to the agent and records that a new turn is outstanding.
func (s *Supervisor) send(ctx context.Context, r *run, msg string) error {
	if err := r.exec.Send(ctx, r.session, msg); err != nil {
		return err
	}
	s.mu.Lock()
	r.turns++
	s.mu.Unlock()
	return nil
}

func digest(r policy.Request) string {
	s := r.Command
	if s == "" && len(r.Paths) > 0 {
		s = r.Paths[0]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
