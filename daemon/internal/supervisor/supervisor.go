// Package supervisor owns the Runs on this machine: it claims work, prepares
// workdirs, starts executors, applies directives, numbers events and enforces policy.
package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/helm/helm/daemon/internal/executor"
	"github.com/helm/helm/daemon/internal/policy"
	"github.com/helm/helm/daemon/internal/wire"
	"github.com/helm/helm/daemon/internal/workspace"
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

	ws      workspace.Workspace
	attempt int
	rawLog  *os.File

	toolUses, hookCalls int // Claude safety check: every tool use must have reached the hook
}

type Supervisor struct {
	ctl       Control
	executors map[string]executor.Executor
	maxRuns   int
	hookSock  string
	log       *slog.Logger

	load interface {
		Sample(context.Context) wire.Load
	}
	wsm *workspace.Manager

	evidenceDir, machine string

	wakeMu sync.Mutex // serializes OnWake so claims never race

	mu       sync.Mutex
	runs     map[string]*run
	lastDirs string
}

type Options struct {
	Executors []executor.Executor
	MaxRuns   int
	HookSock  string
	Log       *slog.Logger
	Load      interface {
		Sample(context.Context) wire.Load
	} // optional heartbeat load sampler
	Workspaces *workspace.Manager // nil: use contract.workdir as is (tests)
	// EvidenceDir stores raw agent output per run (L0); empty disables it.
	EvidenceDir string
	MachineName string // used in ev:// URIs
}

func New(ctl Control, o Options) *Supervisor {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	s := &Supervisor{ctl: ctl, executors: map[string]executor.Executor{}, maxRuns: o.MaxRuns,
		hookSock: o.HookSock, log: o.Log, load: o.Load, wsm: o.Workspaces, evidenceDir: o.EvidenceDir, machine: o.MachineName, runs: map[string]*run{}}
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
		r, ok := s.runs[executor.RunOf(se.Session)]
		current := ok && r.session == se.Session
		s.mu.Unlock()
		if !current {
			continue // unknown run, or a session replaced by a ROLLBACK restart
		}
		if se.ToolUses > 0 {
			s.checkHooked(ctx, r, se.ToolUses)
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

// checkHooked stops a run whose agent used tools that never reached the policy hook — e.g. hooks
// disabled by a managed Claude Code setting. The hook runs before the tool, and the assistant message
// that announces the tool_use is streamed before or around it, so allow a short grace period.
func (s *Supervisor) checkHooked(ctx context.Context, r *run, n int) {
	s.mu.Lock()
	r.toolUses += n
	s.mu.Unlock()
	time.AfterFunc(5*time.Second, func() {
		s.mu.Lock()
		uses, calls, st := r.toolUses, r.hookCalls, r.state
		s.mu.Unlock()
		if calls >= uses || isTerminal(st) {
			return
		}
		s.log.Error("tool calls bypassed the Helm hook; stopping run", "run", r.a.RunID, "tool_uses", uses, "hook_calls", calls)
		s.transition(ctx, r, wire.RunAborted, "policy hook not enforced on this machine (are Claude Code hooks disabled?)")
		_ = r.exec.Stop(ctx, r.session, 5*time.Second)
		s.forget(r.a.RunID)
	})
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
	hb := wire.Heartbeat{RunningRuns: ids}
	if s.load != nil {
		hb.Load = s.load.Sample(context.Background())
	}
	return hb
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
	s.mu.Lock()
	r.hookCalls++
	s.mu.Unlock()
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

	ws := workspace.Workspace{Dir: a.Contract.Workdir}
	if s.wsm != nil {
		var err error
		if ws, err = s.wsm.Prepare(ctx, a.RunID, a.Contract); err != nil {
			s.transition(ctx, r, wire.RunFailed, "workspace: "+err.Error())
			s.forget(a.RunID)
			return
		}
	}
	r.ws = ws
	r.engine = policy.New(ws.Dir, a.Contract.Scope)
	if err := s.launch(ctx, r, a.Contract); err != nil {
		s.transition(ctx, r, wire.RunFailed, err.Error())
		s.forget(a.RunID)
		return
	}
	s.transition(ctx, r, wire.RunRunning, "executor started")
}

// launch starts a fresh agent session for the run (initially, and after ROLLBACK).
func (s *Supervisor) launch(ctx context.Context, r *run, c wire.Contract) error {
	runID := r.a.RunID
	s.mu.Lock()
	r.attempt++
	spec := executor.RunSpec{RunID: runID, Attempt: r.attempt, Workdir: r.ws.Dir, Contract: c, HookSock: s.hookSock,
		Decide: func(req policy.Request) policy.Decision { return s.Decide(runID, req) }}
	s.mu.Unlock()
	if s.evidenceDir != "" {
		name := fmt.Sprintf("transcript-%d.jsonl", spec.Attempt)
		dir := filepath.Join(s.evidenceDir, runID)
		if err := os.MkdirAll(dir, 0o700); err == nil {
			if f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
				s.mu.Lock()
				if r.rawLog != nil {
					_ = r.rawLog.Close()
				}
				r.rawLog = f
				s.mu.Unlock()
				spec.RawLog = f
				s.emit(ctx, r, wire.EvEvidence, wire.Evidence{
					URI: fmt.Sprintf("ev://%s/%s/%s", s.machine, runID, name), Kind: "transcript"})
			}
		}
	}
	sess, err := r.exec.Start(ctx, spec)
	if err != nil {
		return err
	}
	s.mu.Lock()
	r.session = sess
	r.turns = 0
	if c.Prompt != "" {
		r.turns = 1 // the executor sends the contract as the first message
	}
	s.mu.Unlock()
	return nil
}

func (s *Supervisor) forget(runID string) {
	s.mu.Lock()
	if r, ok := s.runs[runID]; ok && r.rawLog != nil {
		_ = r.rawLog.Close()
	}
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
		if u, ok := r.exec.(interface {
			UpdateScope(executor.SessionID, string, wire.Scope)
		}); ok {
			u.UpdateScope(r.session, r.a.Contract.Workdir, *args.Scope) // e.g. Codex sandbox writable roots
		}
		s.mu.Lock()
		r.a.Contract.Version = d.ContractVersion
		r.a.Contract.Scope = *args.Scope
		st := r.state
		s.mu.Unlock()
		if args.Message != "" {
			// An AMEND that answers a gate also releases it: the agent gets a new turn.
			if st == wire.RunGated || st == wire.RunSuspended {
				r.engine.SetPaused(false)
				s.transition(ctx, r, wire.RunRunning, "AMEND")
			}
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
		if s.wsm == nil || r.ws.Snapshot == "" {
			return wire.AckRejectedInvalid, "ROLLBACK needs a git workspace (contract.repo)"
		}
		s.transition(ctx, r, wire.RunPreparing, "ROLLBACK")
		// Detach the old session first so its exit event is recognised as stale by pump.
		s.mu.Lock()
		old := r.session
		r.session = ""
		s.mu.Unlock()
		_ = r.exec.Stop(ctx, old, 10*time.Second)
		if err := s.wsm.Rollback(ctx, r.ws); err != nil {
			s.transition(ctx, r, wire.RunFailed, "rollback: "+err.Error())
			s.forget(r.a.RunID)
			return wire.AckFailed, err.Error()
		}
		r.engine.SetPaused(false)
		c := r.a.Contract
		note := "Your previous attempt was rolled back: all its changes were discarded."
		if args.Message != "" {
			note += " Reviewer note: " + args.Message
		}
		c.Prompt = note + "\n\n" + c.Prompt
		if err := s.launch(ctx, r, c); err != nil {
			s.transition(ctx, r, wire.RunFailed, err.Error())
			s.forget(r.a.RunID)
			return wire.AckFailed, err.Error()
		}
		s.transition(ctx, r, wire.RunRunning, "restarted after ROLLBACK")
	default:
		return wire.AckRejectedInvalid, "unknown directive " + string(d.Kind)
	}
	return wire.AckApplied, ""
}

// send delivers a user message to the agent and records that a new turn is outstanding.
func (s *Supervisor) send(ctx context.Context, r *run, msg string) error {
	newTurn, err := r.exec.Send(ctx, r.session, msg)
	if err != nil {
		return err
	}
	if newTurn {
		s.mu.Lock()
		r.turns++
		s.mu.Unlock()
	}
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
