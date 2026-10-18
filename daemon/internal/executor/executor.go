// Package executor drives agents and processes behind one interface.
// The shape mirrors the Agent Client Protocol (session start / prompt / cancel /
// streamed updates) so an ACP adapter can be added later without touching callers.
package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/helm/helm/daemon/internal/policy"
	"github.com/helm/helm/daemon/internal/wire"
)

type SessionID string

// RunSpec is everything an executor needs to start one Run.
type RunSpec struct {
	RunID    string
	Attempt  int    // bumped on ROLLBACK restarts; sessions are "<run>#<attempt>" so stale events can be told apart
	Workdir  string // prepared git worktree or contract workdir
	Contract wire.Contract
	HookSock string // unix socket of the policy engine (claude executor wires it into PreToolUse)
	// Decide is the in-process policy check (codex executor answers app-server approval requests with it).
	Decide    func(policy.Request) policy.Decision
	SkillDirs []string // skill folders to expose to the agent for this run only
	ResumeRef string   // agent-native session id to resume (ROLLBACK starts fresh instead)
	Env       []string
}

// Event is a normalized L1 event before the supervisor assigns it a seq.
type Event struct {
	Type wire.EventType
	Data any
	TS   time.Time
}

type Executor interface {
	Name() string
	Start(ctx context.Context, spec RunSpec) (SessionID, error)
	// Send delivers a user message (STEER, AMEND explanations, CONTINUE notes). newTurn reports
	// whether it will produce its own turn result (Claude queues a new turn; Codex merges the
	// message into a turn that is still running). The supervisor counts outstanding turns with it.
	Send(ctx context.Context, s SessionID, msg string) (newTurn bool, err error)
	// Interrupt cancels the current turn but keeps the session.
	Interrupt(ctx context.Context, s SessionID) error
	// Stop ends the session; after grace the process group is killed.
	Stop(ctx context.Context, s SessionID, grace time.Duration) error
	// Events streams normalized events for all sessions of this executor; closed on shutdown.
	Events() <-chan SessionEvent
	// NativeRef returns the agent-native session id (Codex thread id, Claude session id) for resume.
	NativeRef(s SessionID) string
}

type SessionEvent struct {
	Session SessionID
	Event   Event
	Done    bool  // session ended
	Err     error // set when Done because of a failure
}

// SessionFor names a session; the supervisor parses the run id back out with RunOf.
func SessionFor(spec RunSpec) SessionID {
	return SessionID(fmt.Sprintf("%s#%d", spec.RunID, spec.Attempt))
}

// RunOf returns the run id of a session.
func RunOf(s SessionID) string {
	if i := strings.LastIndexByte(string(s), '#'); i >= 0 {
		return string(s)[:i]
	}
	return string(s)
}
