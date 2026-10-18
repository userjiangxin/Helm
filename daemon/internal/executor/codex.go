package executor

import (
	"context"
	"errors"
	"time"
)

// Codex drives `codex app-server` over stdio JSON-RPC (one process per run).
//
// M0 implementation notes (see docs/m0-tasks.md, task D4):
//   - Generate the protocol types once with `codex app-server generate-json-schema --out ./third_party/codex-schema`
//     and keep them in git; regenerate on every Codex upgrade.
//   - Handshake: initialize (clientInfo {name:"helmd"}) -> thread/start (cwd = spec.Workdir) -> turn/start (prompt).
//   - Normalize server notifications (item/* , turn/completed) into L1 events.
//   - Server-initiated approval requests -> policy.Engine.Decide -> respond accept/decline;
//     Hold => decline with a "waiting for Helm approval" reason and emit tool_call{decision:hold}.
//   - Interrupt -> turn/interrupt (bounded wait ~2s, then kill the process group).
//   - NativeRef = thread id; resume with thread/resume.
type Codex struct{ events chan SessionEvent }

func NewCodex() *Codex { return &Codex{events: make(chan SessionEvent, 256)} }

func (c *Codex) Name() string                 { return "codex" }
func (c *Codex) Events() <-chan SessionEvent  { return c.events }
func (c *Codex) NativeRef(s SessionID) string { return "" }
func (c *Codex) Start(ctx context.Context, spec RunSpec) (SessionID, error) {
	return "", errors.New("codex executor: not implemented (M0 task D4)")
}
func (c *Codex) Send(ctx context.Context, s SessionID, msg string) error      { return errNotImpl }
func (c *Codex) Interrupt(ctx context.Context, s SessionID) error             { return errNotImpl }
func (c *Codex) Stop(ctx context.Context, s SessionID, g time.Duration) error { return errNotImpl }

var errNotImpl = errors.New("not implemented")
