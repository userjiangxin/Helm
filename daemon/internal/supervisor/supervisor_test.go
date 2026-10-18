package supervisor

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/helm/helm/daemon/internal/executor"
	"github.com/helm/helm/daemon/internal/policy"
	"github.com/helm/helm/daemon/internal/wire"
)

type fakeCtl struct {
	mu      sync.Mutex
	queue   []wire.RunAssignment
	dirs    []wire.Directive
	acks    []wire.DirectiveAck
	events  []wire.Event
	claimed int
}

func (f *fakeCtl) Claim(_ context.Context, slots int) ([]wire.RunAssignment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := min(slots, len(f.queue))
	out := f.queue[:n]
	f.queue = f.queue[n:]
	f.claimed += n
	return out, nil
}
func (f *fakeCtl) Directives(_ context.Context, after string) ([]wire.Directive, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.dirs
	f.dirs = nil
	return out, nil
}
func (f *fakeCtl) Ack(_ context.Context, a wire.DirectiveAck) error {
	f.mu.Lock()
	f.acks = append(f.acks, a)
	f.mu.Unlock()
	return nil
}
func (f *fakeCtl) Publish(_ context.Context, b wire.RunEvents) {
	f.mu.Lock()
	f.events = append(f.events, b.Events...)
	f.mu.Unlock()
}
func (f *fakeCtl) lastState() wire.RunState {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.events) - 1; i >= 0; i-- {
		if f.events[i].Type == wire.EvStateChange {
			var sc wire.StateChange
			_ = json.Unmarshal(f.events[i].Data, &sc)
			return sc.To
		}
	}
	return ""
}

type fakeExec struct {
	mu   sync.Mutex
	sent []string
	ev   chan executor.SessionEvent
}

func (e *fakeExec) Name() string { return "codex" }
func (e *fakeExec) Start(context.Context, executor.RunSpec) (executor.SessionID, error) {
	return "s1", nil
}
func (e *fakeExec) Send(_ context.Context, _ executor.SessionID, m string) error {
	e.mu.Lock()
	e.sent = append(e.sent, m)
	e.mu.Unlock()
	return nil
}
func (e *fakeExec) Interrupt(context.Context, executor.SessionID) error           { return nil }
func (e *fakeExec) Stop(context.Context, executor.SessionID, time.Duration) error { return nil }
func (e *fakeExec) Events() <-chan executor.SessionEvent {
	if e.ev == nil {
		return nil
	}
	return e.ev
}
func (e *fakeExec) NativeRef(executor.SessionID) string { return "" }

func assignment(id string, version int) wire.RunAssignment {
	return wire.RunAssignment{RunID: id, Epoch: 1, Executor: "codex", Contract: wire.Contract{
		Version: version, Workdir: "/w/" + id, Scope: wire.Scope{WritePaths: []string{"/data/ok/**"}}}}
}

func TestLifecycleGateAndDirectives(t *testing.T) {
	ctx := context.Background()
	ctl := &fakeCtl{queue: []wire.RunAssignment{assignment("r1", 2), assignment("r2", 1), assignment("r3", 1)}}
	ex := &fakeExec{}
	s := New(ctl, Options{Executors: []executor.Executor{ex}, MaxRuns: 2})

	s.OnWake(ctx, "test")
	if ctl.claimed != 2 {
		t.Fatalf("must respect MaxRuns: claimed %d", ctl.claimed)
	}
	if ctl.lastState() != wire.RunRunning {
		t.Fatalf("expected RUNNING, got %s", ctl.lastState())
	}

	// Out-of-scope write is held and moves the run to GATED.
	if d := s.Decide("r1", policy.Request{Tool: "Write", Paths: []string{"/etc/x"}}); d.Action != policy.Hold {
		t.Fatalf("got %s", d.Action)
	}
	if ctl.lastState() != wire.RunGated {
		t.Fatalf("expected GATED, got %s", ctl.lastState())
	}

	// Stale directive (v1 < run v2) is rejected; AMEND v3 widens scope; CONTINUE resumes with a note.
	amendArgs, _ := json.Marshal(map[string]any{"scope": wire.Scope{WritePaths: []string{"/etc/**"}}, "message": "ok to write /etc"})
	contArgs, _ := json.Marshal(map[string]string{"message": "approved, go on"})
	ctl.dirs = []wire.Directive{
		{ID: "d1", RunID: "r1", Kind: wire.DirPause, ContractVersion: 1},
		{ID: "d2", RunID: "r1", Kind: wire.DirAmend, ContractVersion: 3, Args: amendArgs},
		{ID: "d3", RunID: "r1", Kind: wire.DirContinue, ContractVersion: 3, Args: contArgs},
		{ID: "d4", RunID: "nope", Kind: wire.DirAbort, ContractVersion: 1},
	}
	s.OnWake(ctx, "test")
	want := []wire.AckStatus{wire.AckRejectedStale, wire.AckApplied, wire.AckApplied, wire.AckRejectedInvalid}
	for i, a := range ctl.acks {
		if a.Status != want[i] {
			t.Errorf("ack %s: got %s (%s), want %s", a.DirectiveID, a.Status, a.Detail, want[i])
		}
	}
	if d := s.Decide("r1", policy.Request{Tool: "Write", Paths: []string{"/etc/x"}}); d.Action != policy.Allow {
		t.Fatalf("after AMEND: got %s", d.Action)
	}
	if len(ex.sent) != 2 {
		t.Fatalf("expected AMEND and CONTINUE messages injected, got %v", ex.sent)
	}

	// PAUSE at current version denies everything.
	ctl.dirs = []wire.Directive{{ID: "d5", RunID: "r1", Kind: wire.DirPause, ContractVersion: 3}}
	s.OnWake(ctx, "test")
	if d := s.Decide("r1", policy.Request{Tool: "Bash", Command: "ls"}); d.Action != policy.Deny {
		t.Fatalf("paused: got %s", d.Action)
	}

	// Event seq numbers are strictly increasing per run (r1's events are the majority here;
	// the fake control plane flattens runs, so check the sequence is never repeated per run).
	if ctl.events[0].Seq != 1 {
		t.Fatalf("first seq must be 1, got %d", ctl.events[0].Seq)
	}
}

func TestUnknownExecutorFails(t *testing.T) {
	a := assignment("r9", 1)
	a.Executor = "cursor"
	ctl := &fakeCtl{queue: []wire.RunAssignment{a}}
	s := New(ctl, Options{Executors: []executor.Executor{&fakeExec{}}, MaxRuns: 1})
	s.OnWake(context.Background(), "test")
	if ctl.lastState() != wire.RunFailed || s.active() != 0 {
		t.Fatalf("expected FAILED and slot freed, got %s / %d", ctl.lastState(), s.active())
	}
}

func TestLateClaimDoesNotSkipNewerInstructions(t *testing.T) {
	ctx := context.Background()
	a := assignment("r1", 1)
	a.Contract.Prompt = "do it"
	ctl := &fakeCtl{queue: []wire.RunAssignment{a}}
	ex := &fakeExec{ev: make(chan executor.SessionEvent, 8)}
	s := New(ctl, Options{Executors: []executor.Executor{ex}, MaxRuns: 1})
	s.OnWake(ctx, "test")

	claim := func() {
		ex.ev <- executor.SessionEvent{Session: "r1", Event: executor.Event{Type: wire.EvClaim, Data: wire.Claim{Kind: "done", Text: "ok"}}}
	}
	waitEvents := func(n int) {
		for i := 0; i < 200; i++ {
			ctl.mu.Lock()
			got := len(ctl.events)
			ctl.mu.Unlock()
			if got >= n {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("expected %d events", n)
	}

	// A STEER is sent before the first turn's result arrives.
	msg, _ := json.Marshal(map[string]string{"message": "also check label_v2"})
	ctl.dirs = []wire.Directive{{ID: "d1", RunID: "r1", Kind: wire.DirSteer, ContractVersion: 1, Args: msg}}
	s.OnWake(ctx, "test")
	base := len(ctl.events)

	claim() // late result of turn 1
	waitEvents(base + 1)
	if st := ctl.lastState(); st != wire.RunRunning {
		t.Fatalf("turn-1 claim must not advance the run while the STEER is pending, got %s", st)
	}
	claim() // result of the STEER turn
	waitEvents(base + 3)
	if st := ctl.lastState(); st != wire.RunVerifying {
		t.Fatalf("expected VERIFYING after the latest turn, got %s", st)
	}
}
