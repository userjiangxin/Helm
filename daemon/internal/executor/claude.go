package executor

import (
	"context"
	"errors"
	"time"
)

// Claude drives Claude Code in headless streaming mode (one process per run).
//
// M0 implementation notes (see docs/m0-tasks.md, task D5):
//   - Launch: claude -p --input-format stream-json --output-format stream-json --verbose
//     with cwd = spec.Workdir and a per-run settings file (--settings) that registers
//     a PreToolUse hook: `helmd hook pre-tool-use --run <run_id> --sock <spec.HookSock>`
//     (matcher "*"). The hook package already implements and tests that bridge.
//   - Send = write a user message line to stdin; Interrupt = control request / SIGINT;
//     Stop = close stdin, then SIGTERM/SIGKILL the process group after grace.
//   - Normalize stream-json lines (assistant text, tool_use, tool_result, result) into L1 events;
//     the final `result` line carries usage for token budgets.
//   - NativeRef = session_id from the init line; resume with --resume.
//   - Verify flag names against `claude --help` of the installed version before coding.
type Claude struct{ events chan SessionEvent }

func NewClaude() *Claude { return &Claude{events: make(chan SessionEvent, 256)} }

func (c *Claude) Name() string                 { return "claude" }
func (c *Claude) Events() <-chan SessionEvent  { return c.events }
func (c *Claude) NativeRef(s SessionID) string { return "" }
func (c *Claude) Start(ctx context.Context, spec RunSpec) (SessionID, error) {
	return "", errors.New("claude executor: not implemented (M0 task D5)")
}
func (c *Claude) Send(ctx context.Context, s SessionID, msg string) error      { return errNotImpl }
func (c *Claude) Interrupt(ctx context.Context, s SessionID) error             { return errNotImpl }
func (c *Claude) Stop(ctx context.Context, s SessionID, g time.Duration) error { return errNotImpl }
