package executor

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/helm/helm/daemon/internal/wire"
)

// Claude drives Claude Code headless: `claude -p --input-format stream-json --output-format stream-json`.
// One process per Run; stdin stays open so later user messages (STEER, CONTINUE notes) start new turns.
//
// Control is enforced by a per-run settings file registering `helmd hook pre-tool-use` for every
// tool (matcher "*"). The session runs in bypassPermissions mode on purpose: Claude Code applies
// PreToolUse denials even in that mode, so the Helm policy engine is the single gate and Claude
// never stalls waiting for an interactive permission prompt nobody can answer.
type Claude struct {
	// Bin is the claude executable; HelmdBin is the helmd executable used in the hook command.
	Bin, HelmdBin string
	// StateDir holds per-run settings files (e.g. ~/.helm/run).
	StateDir string
	// PermissionMode defaults to bypassPermissions (see above).
	PermissionMode string

	events chan SessionEvent
	mu     sync.Mutex
	procs  map[SessionID]*claudeProc
}

type claudeProc struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	writeMu   sync.Mutex
	sessionID string
	done      chan struct{}
}

func NewClaude(bin, helmdBin, stateDir string) *Claude {
	return &Claude{Bin: bin, HelmdBin: helmdBin, StateDir: stateDir, PermissionMode: "bypassPermissions",
		events: make(chan SessionEvent, 1024), procs: map[SessionID]*claudeProc{}}
}

func (c *Claude) Name() string                { return "claude" }
func (c *Claude) Events() <-chan SessionEvent { return c.events }

func (c *Claude) NativeRef(s SessionID) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p, ok := c.procs[s]; ok {
		return p.sessionID
	}
	return ""
}

func (c *Claude) settingsFile(spec RunSpec) (string, error) {
	dir := filepath.Join(c.StateDir, spec.RunID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	hookCmd := fmt.Sprintf("%s hook pre-tool-use --run %s --sock %s", shellQuote(c.HelmdBin), shellQuote(spec.RunID), shellQuote(spec.HookSock))
	settings := map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []any{map[string]any{
				"matcher": "*",
				"hooks":   []any{map[string]any{"type": "command", "command": hookCmd, "timeout": 15}},
			}},
		},
	}
	b, _ := json.MarshalIndent(settings, "", "  ")
	p := filepath.Join(dir, "claude-settings.json")
	return p, os.WriteFile(p, b, 0o600)
}

// Start launches Claude Code for one run and sends the contract as the first user message.
func (c *Claude) Start(ctx context.Context, spec RunSpec) (SessionID, error) {
	if spec.HookSock == "" {
		return "", errors.New("claude executor: refusing to start without a policy hook socket")
	}
	settings, err := c.settingsFile(spec)
	if err != nil {
		return "", err
	}
	nativeID := spec.ResumeRef
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--settings", settings, "--permission-mode", c.PermissionMode,
		"--append-system-prompt", helmSystemPrompt}
	if nativeID != "" {
		args = append(args, "--resume", nativeID)
	} else {
		nativeID = newUUID()
		args = append(args, "--session-id", nativeID)
	}
	// Not CommandContext: a run outlives the claim request that started it. Stop() ends it.
	cmd := exec.Command(c.Bin, args...)
	cmd.Dir = spec.Workdir
	cmd.Env = append(cleanEnv(os.Environ()), spec.Env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderr := &tailBuffer{max: 4096}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start claude: %w", err)
	}
	sid := SessionID(spec.RunID)
	p := &claudeProc{cmd: cmd, stdin: stdin, sessionID: nativeID, done: make(chan struct{})}
	c.mu.Lock()
	c.procs[sid] = p
	c.mu.Unlock()

	go c.read(sid, stdout)
	go func() {
		err := cmd.Wait()
		if err != nil && stderr.String() != "" {
			err = fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
		}
		close(p.done)
		c.events <- SessionEvent{Session: sid, Done: true, Err: err}
		c.mu.Lock()
		delete(c.procs, sid)
		c.mu.Unlock()
	}()

	if spec.ResumeRef == "" {
		if _, err := c.Send(ctx, sid, InitialPrompt(spec.Contract)); err != nil {
			_ = c.Stop(ctx, sid, time.Second)
			return "", err
		}
	}
	return sid, nil
}

func (c *Claude) proc(s SessionID) (*claudeProc, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.procs[s]
	if !ok {
		return nil, fmt.Errorf("claude session %s is not running", s)
	}
	return p, nil
}

func (c *Claude) writeLine(s SessionID, v any) error {
	p, err := c.proc(s)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(v)
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_, err = p.stdin.Write(append(b, '\n'))
	return err
}

// Send queues a user message; Claude Code processes it as its own next turn.
func (c *Claude) Send(_ context.Context, s SessionID, msg string) (bool, error) {
	err := c.writeLine(s, map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": msg},
	})
	return err == nil, err
}

// Interrupt cancels the current turn via the stream-json control channel.
func (c *Claude) Interrupt(_ context.Context, s SessionID) error {
	return c.writeLine(s, map[string]any{
		"type":       "control_request",
		"request_id": "helm-" + newUUID(),
		"request":    map[string]any{"subtype": "interrupt"},
	})
}

// Stop closes stdin (Claude exits after the current turn), then escalates to signals.
func (c *Claude) Stop(_ context.Context, s SessionID, grace time.Duration) error {
	p, err := c.proc(s)
	if err != nil {
		return nil // already gone
	}
	_ = p.stdin.Close()
	select {
	case <-p.done:
		return nil
	case <-time.After(grace):
	}
	pgid := -p.cmd.Process.Pid
	_ = syscall.Kill(pgid, syscall.SIGTERM)
	select {
	case <-p.done:
		return nil
	case <-time.After(5 * time.Second):
	}
	_ = syscall.Kill(pgid, syscall.SIGKILL)
	<-p.done
	return nil
}

// ---------- stream-json normalization ----------

type ccLine struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	Message   *ccMessage      `json:"message"`
	Result    string          `json:"result"`
	IsError   bool            `json:"is_error"`
	Usage     json.RawMessage `json:"usage"`
}

type ccMessage struct {
	Content json.RawMessage `json:"content"`
}

type ccBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	IsError bool            `json:"is_error"`
	Content json.RawMessage `json:"content"`
}

func (c *Claude) read(sid SessionID, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		for _, ev := range NormalizeClaudeLine(sc.Bytes()) {
			c.events <- SessionEvent{Session: sid, Event: ev}
		}
	}
}

// NormalizeClaudeLine maps one stream-json line to zero or more L1 events.
// Tool calls are not emitted here: the PreToolUse hook already reports every call with its
// policy decision, which is the more useful record.
func NormalizeClaudeLine(line []byte) []Event {
	var l ccLine
	if err := json.Unmarshal(line, &l); err != nil {
		return nil
	}
	now := time.Now().UTC()
	switch l.Type {
	case "assistant":
		var out []Event
		for _, b := range blocks(l.Message) {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				out = append(out, Event{Type: wire.EvAgentMessage, TS: now,
					Data: wire.AgentMessage{Role: "assistant", Text: truncate(b.Text, 4000)}})
			}
		}
		return out
	case "user":
		var out []Event
		for _, b := range blocks(l.Message) {
			if b.Type == "tool_result" && b.IsError {
				msg := blockText(b.Content)
				if strings.Contains(msg, "PreToolUse") && strings.Contains(msg, "Helm:") {
					continue // our own denial; already recorded as a tool_call with decision deny/hold
				}
				out = append(out, Event{Type: wire.EvError, TS: now,
					Data: wire.ErrorSig{Signature: ErrorSignature(msg), Message: truncate(msg, 2000), Count: 1}})
			}
		}
		return out
	case "result":
		kind := "done"
		if l.IsError || (l.Subtype != "" && l.Subtype != "success") {
			kind = "error"
		}
		return []Event{{Type: wire.EvClaim, TS: now, Data: wire.Claim{Kind: kind, Text: truncate(l.Result, 4000)}}}
	}
	return nil
}

func blocks(m *ccMessage) []ccBlock {
	if m == nil {
		return nil
	}
	var bs []ccBlock
	if err := json.Unmarshal(m.Content, &bs); err != nil {
		var s string
		if json.Unmarshal(m.Content, &s) == nil && s != "" {
			return []ccBlock{{Type: "text", Text: s}}
		}
	}
	return bs
}

func blockText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &parts)
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// ---------- helpers ----------

const helmSystemPrompt = `You are running under Helm, a supervisor that enforces a task contract.
Some tool calls may be denied with a reason that starts with "Helm:". Treat such a denial as final:
do not retry the same action and do not work around it with another tool. Continue with the parts of the
task you can do, then stop and explain what is blocked. When you believe the task is complete, say so and
list how each acceptance criterion was checked.`

// InitialPrompt renders the contract as the first user message.
func InitialPrompt(c wire.Contract) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Task\n%s\n\n%s\n", c.Intent, c.Prompt)
	if len(c.Acceptance) > 0 {
		b.WriteString("\n# Acceptance criteria (all must hold before you report completion)\n")
		for _, a := range c.Acceptance {
			fmt.Fprintf(&b, "- %s\n", a)
		}
	}
	if len(c.Scope.WritePaths) > 0 || len(c.Scope.ReadonlyPaths) > 0 {
		b.WriteString("\n# Scope (enforced)\nYou may write inside the current working directory")
		if len(c.Scope.WritePaths) > 0 {
			fmt.Fprintf(&b, " and: %s", strings.Join(c.Scope.WritePaths, ", "))
		}
		b.WriteString(".\n")
		if len(c.Scope.ReadonlyPaths) > 0 {
			fmt.Fprintf(&b, "Never modify: %s\n", strings.Join(c.Scope.ReadonlyPaths, ", "))
		}
	}
	return b.String()
}

// cleanEnv drops variables that would make a child Claude Code attach to a parent session.
func cleanEnv(env []string) []string {
	drop := []string{"CLAUDECODE=", "CLAUDE_CODE_SESSION_ID=", "CLAUDE_CODE_REMOTE_SESSION_ID=",
		"CLAUDE_CODE_CHILD_SESSION=", "CLAUDE_CODE_INCLUDE_PARTIAL_MESSAGES=", "CLAUDE_CODE_ENTRYPOINT="}
	out := env[:0:0]
outer:
	for _, kv := range env {
		for _, d := range drop {
			if strings.HasPrefix(kv, d) {
				continue outer
			}
		}
		out = append(out, kv)
	}
	return out
}

func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " '\"\\$`!*?;&|<>(){}[]~#\t\n") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
