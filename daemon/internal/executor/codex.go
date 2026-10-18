package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/helm/helm/daemon/internal/policy"
	"github.com/helm/helm/daemon/internal/wire"
)

// Codex drives `codex app-server --stdio` (newline-delimited JSON-RPC, no "jsonrpc" header).
// Protocol types: `codex app-server generate-ts` (verified against codex-cli 0.163.0).
//
// Control model:
//   - The thread runs in the workspace-write sandbox, so writes inside the workdir and the
//     contract's write roots succeed natively; anything else needs an approval.
//   - approvalPolicy "untrusted": Codex asks before commands it does not consider safe.
//   - Every approval request (item/commandExecution/requestApproval, item/fileChange/requestApproval)
//     is answered by the Helm policy engine: allow → "accept"; deny/hold → "decline". A hold also
//     moves the run to GATED (the supervisor's Decide does that).
type Codex struct {
	Bin string

	events chan SessionEvent
	mu     sync.Mutex
	procs  map[SessionID]*codexProc
}

type codexProc struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	writeMu sync.Mutex
	nextID  atomic.Int64
	done    chan struct{}

	mu       sync.Mutex
	pending  map[int64]chan rpcResponse
	threadID string
	turnID   string              // active turn, "" when idle
	items    map[string][]string // fileChange itemId → paths (approval requests carry only the id)
	lastText string              // last agentMessage of the active turn → claim text
	decide   func(policy.Request) policy.Decision
	sandbox  map[string]any
	seenErr  map[string]bool // error signatures already reported this turn (Codex retries loudly)
}

type rpcResponse struct {
	Result json.RawMessage
	Err    *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
}

func NewCodex(bin string) *Codex {
	return &Codex{Bin: bin, events: make(chan SessionEvent, 1024), procs: map[SessionID]*codexProc{}}
}

func (c *Codex) Name() string                { return "codex" }
func (c *Codex) Events() <-chan SessionEvent { return c.events }

func (c *Codex) NativeRef(s SessionID) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p, ok := c.procs[s]; ok {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.threadID
	}
	return ""
}

// writableRoots turns contract write globs into sandbox roots ("/data/x/**" → "/data/x").
// Globs with wildcards before the last segment cannot be expressed as a root and are left to approvals.
func writableRoots(workdir string, globs []string) []string {
	roots := []string{}
	if workdir != "" {
		roots = append(roots, workdir)
	}
	for _, g := range globs {
		g = strings.TrimSuffix(strings.TrimSuffix(g, "/**"), "/*")
		if strings.HasPrefix(g, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				g = home + g[1:]
			}
		}
		if g != "" && !strings.ContainsAny(g, "*?[") {
			roots = append(roots, g)
		}
	}
	return roots
}

func sandboxPolicy(workdir string, scope wire.Scope) map[string]any {
	return map[string]any{
		"type":                "workspaceWrite",
		"writableRoots":       writableRoots(workdir, scope.WritePaths),
		"networkAccess":       true, // data downloads; risky commands are still gated by approvals
		"excludeTmpdirEnvVar": false,
		"excludeSlashTmp":     false,
	}
}

func (c *Codex) Start(ctx context.Context, spec RunSpec) (SessionID, error) {
	if spec.Decide == nil {
		return "", errors.New("codex executor: refusing to start without a policy decider")
	}
	cmd := exec.Command(c.Bin, "app-server", "--stdio")
	cmd.Dir = spec.Workdir
	cmd.Env = append(os.Environ(), spec.Env...)
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
		return "", fmt.Errorf("start codex: %w", err)
	}
	sid := SessionFor(spec)
	p := &codexProc{cmd: cmd, stdin: stdin, done: make(chan struct{}), pending: map[int64]chan rpcResponse{},
		items: map[string][]string{}, seenErr: map[string]bool{}, decide: spec.Decide, sandbox: sandboxPolicy(spec.Workdir, spec.Contract.Scope)}
	c.mu.Lock()
	c.procs[sid] = p
	c.mu.Unlock()

	go c.read(sid, p, stdout, spec.RawLog)
	go func() {
		err := cmd.Wait()
		if err != nil && stderr.String() != "" {
			err = fmt.Errorf("%w: %s", err, lastLines(stderr.String(), 3))
		}
		close(p.done)
		p.mu.Lock()
		for _, ch := range p.pending {
			close(ch)
		}
		p.pending = map[int64]chan rpcResponse{}
		p.mu.Unlock()
		c.events <- SessionEvent{Session: sid, Done: true, Err: err}
		c.mu.Lock()
		delete(c.procs, sid)
		c.mu.Unlock()
	}()

	fail := func(err error) (SessionID, error) {
		_ = c.Stop(ctx, sid, time.Second)
		return "", err
	}
	hctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if _, err := p.call(hctx, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "helmd", "title": "Helm", "version": "0.1"},
		"capabilities": nil,
	}); err != nil {
		return fail(fmt.Errorf("codex initialize: %w", err))
	}
	if err := p.notify("initialized", nil); err != nil {
		return fail(err)
	}
	var threadRes struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	method, params := "thread/start", map[string]any{
		"cwd": spec.Workdir, "approvalPolicy": "untrusted", "sandbox": "workspace-write",
		"developerInstructions": helmSystemPrompt,
	}
	if spec.ResumeRef != "" {
		method = "thread/resume"
		params["threadId"] = spec.ResumeRef
		params["excludeTurns"] = true
	}
	raw, err := p.call(hctx, method, params)
	if err != nil {
		return fail(fmt.Errorf("codex %s: %w", method, err))
	}
	if err := json.Unmarshal(raw, &threadRes); err != nil || threadRes.Thread.ID == "" {
		return fail(fmt.Errorf("codex %s: unexpected response %s", method, truncate(string(raw), 300)))
	}
	p.mu.Lock()
	p.threadID = threadRes.Thread.ID
	p.mu.Unlock()

	if spec.ResumeRef == "" {
		if _, err := c.Send(ctx, sid, InitialPrompt(spec.Contract)); err != nil {
			return fail(err)
		}
	}
	return sid, nil
}

func (c *Codex) proc(s SessionID) (*codexProc, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.procs[s]
	if !ok {
		return nil, fmt.Errorf("codex session %s is not running", s)
	}
	return p, nil
}

// Send starts a turn, or adds input to the running turn (then no separate turn result follows).
func (c *Codex) Send(ctx context.Context, s SessionID, msg string) (bool, error) {
	p, err := c.proc(s)
	if err != nil {
		return false, err
	}
	p.mu.Lock()
	thread, active, sandbox := p.threadID, p.turnID != "", p.sandbox
	p.mu.Unlock()
	params := map[string]any{
		"threadId":      thread,
		"input":         []any{map[string]any{"type": "text", "text": msg, "text_elements": []any{}}},
		"sandboxPolicy": sandbox,
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	raw, err := p.call(cctx, "turn/start", params)
	if err != nil {
		return false, fmt.Errorf("codex turn/start: %w", err)
	}
	var res struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	_ = json.Unmarshal(raw, &res)
	if !active && res.Turn.ID != "" {
		p.mu.Lock()
		p.turnID = res.Turn.ID
		p.mu.Unlock()
	}
	return !active, nil
}

// UpdateScope is used on AMEND so the next turn runs with the new writable roots.
func (c *Codex) UpdateScope(s SessionID, workdir string, scope wire.Scope) {
	if p, err := c.proc(s); err == nil {
		p.mu.Lock()
		p.sandbox = sandboxPolicy(workdir, scope)
		p.mu.Unlock()
	}
}

func (c *Codex) Interrupt(ctx context.Context, s SessionID) error {
	p, err := c.proc(s)
	if err != nil {
		return err
	}
	p.mu.Lock()
	thread, turn := p.threadID, p.turnID
	p.mu.Unlock()
	if turn == "" {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = p.call(cctx, "turn/interrupt", map[string]any{"threadId": thread, "turnId": turn})
	return err
}

func (c *Codex) Stop(ctx context.Context, s SessionID, grace time.Duration) error {
	p, err := c.proc(s)
	if err != nil {
		return nil
	}
	_ = c.Interrupt(ctx, s)
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

// ---------- JSON-RPC plumbing ----------

func (p *codexProc) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_, err = p.stdin.Write(append(b, '\n'))
	return err
}

func (p *codexProc) notify(method string, params any) error {
	m := map[string]any{"method": method}
	if params != nil {
		m["params"] = params
	}
	return p.write(m)
}

func (p *codexProc) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := p.nextID.Add(1)
	ch := make(chan rpcResponse, 1)
	p.mu.Lock()
	p.pending[id] = ch
	p.mu.Unlock()
	if err := p.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return nil, errors.New("codex app-server exited")
		}
		if r.Err != nil {
			return nil, fmt.Errorf("%s (code %d)", r.Err.Message, r.Err.Code)
		}
		return r.Result, nil
	case <-ctx.Done():
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return nil, ctx.Err()
	}
}

type rpcMsg struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Codex) read(sid SessionID, p *codexProc, r io.Reader, raw io.Writer) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 32<<20)
	for sc.Scan() {
		writeRaw(raw, sc.Bytes())
		var m rpcMsg
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch {
		case m.Method == "" && len(m.ID) > 0: // response
			var id int64
			if json.Unmarshal(m.ID, &id) != nil {
				continue
			}
			p.mu.Lock()
			ch, ok := p.pending[id]
			delete(p.pending, id)
			p.mu.Unlock()
			if ok {
				ch <- rpcResponse{Result: m.Result, Err: m.Error}
			}
		case m.Method != "" && len(m.ID) > 0: // server → client request
			go c.answer(p, m)
		case m.Method != "": // notification
			for _, ev := range p.normalize(m.Method, m.Params) {
				c.events <- SessionEvent{Session: sid, Event: ev}
			}
		}
	}
}

// answer resolves server requests. Approvals go through the Helm policy engine.
func (c *Codex) answer(p *codexProc, m rpcMsg) {
	reply := func(result any) { _ = p.write(map[string]any{"id": m.ID, "result": result}) }
	decision := func(d policy.Decision) string {
		if d.Action == policy.Allow {
			return "accept"
		}
		return "decline"
	}
	switch m.Method {
	case "item/commandExecution/requestApproval":
		var prm struct {
			Command string `json:"command"`
			Cwd     string `json:"cwd"`
		}
		_ = json.Unmarshal(m.Params, &prm)
		reply(map[string]any{"decision": decision(p.decide(policy.Request{Tool: "exec", Command: prm.Command}))})
	case "item/fileChange/requestApproval":
		var prm struct {
			ItemID    string  `json:"itemId"`
			GrantRoot *string `json:"grantRoot"`
		}
		_ = json.Unmarshal(m.Params, &prm)
		p.mu.Lock()
		paths := append([]string(nil), p.items[prm.ItemID]...)
		p.mu.Unlock()
		if prm.GrantRoot != nil && *prm.GrantRoot != "" {
			paths = append(paths, *prm.GrantRoot)
		}
		if len(paths) == 0 {
			// Fail closed: we cannot tell what would be written.
			reply(map[string]any{"decision": "decline"})
			return
		}
		reply(map[string]any{"decision": decision(p.decide(policy.Request{Tool: "apply_patch", Paths: paths}))})
	default:
		// User-input prompts, MCP elicitations, permission escalations, legacy approvals:
		// nobody is at a keyboard for this agent, so refuse explicitly.
		_ = p.write(map[string]any{"id": m.ID, "error": map[string]any{"code": -32601, "message": "not supported by Helm: " + m.Method}})
	}
}

// normalize maps app-server notifications to L1 events and tracks turn state.
func (p *codexProc) normalize(method string, raw json.RawMessage) []Event {
	now := time.Now().UTC()
	switch method {
	case "item/started", "item/completed":
		var n struct {
			Item struct {
				Type             string `json:"type"`
				ID               string `json:"id"`
				Text             string `json:"text"`
				Command          string `json:"command"`
				ExitCode         *int   `json:"exitCode"`
				DurationMS       *int64 `json:"durationMs"`
				AggregatedOutput string `json:"aggregatedOutput"`
				Status           string `json:"status"`
				Changes          []struct {
					Path string `json:"path"`
					Diff string `json:"diff"`
				} `json:"changes"`
			} `json:"item"`
		}
		if json.Unmarshal(raw, &n) != nil {
			return nil
		}
		it := n.Item
		if method == "item/started" {
			if it.Type == "fileChange" {
				paths := make([]string, 0, len(it.Changes))
				for _, ch := range it.Changes {
					paths = append(paths, ch.Path)
				}
				p.mu.Lock()
				p.items[it.ID] = paths
				p.mu.Unlock()
			}
			return nil
		}
		switch it.Type {
		case "agentMessage":
			p.mu.Lock()
			p.lastText = it.Text
			p.mu.Unlock()
			if strings.TrimSpace(it.Text) == "" {
				return nil
			}
			return []Event{{Type: wire.EvAgentMessage, TS: now, Data: wire.AgentMessage{Role: "assistant", Text: truncate(it.Text, 4000)}}}
		case "commandExecution":
			if it.ExitCode == nil {
				return nil
			}
			out := []Event{{Type: wire.EvCmdExit, TS: now, Data: wire.CmdExit{
				CmdDigest: truncate(it.Command, 200), ExitCode: *it.ExitCode, DurationMS: deref(it.DurationMS)}}}
			if *it.ExitCode != 0 && strings.TrimSpace(it.AggregatedOutput) != "" {
				msg := lastLines(it.AggregatedOutput, 5)
				out = append(out, Event{Type: wire.EvError, TS: now,
					Data: wire.ErrorSig{Signature: ErrorSignature(msg), Message: truncate(msg, 2000), Count: 1}})
			}
			return out
		case "fileChange":
			p.mu.Lock()
			delete(p.items, it.ID)
			p.mu.Unlock()
			if it.Status != "completed" {
				return nil
			}
			out := make([]Event, 0, len(it.Changes))
			for _, ch := range it.Changes {
				add, del := diffStat(ch.Diff)
				out = append(out, Event{Type: wire.EvFSChange, TS: now, Data: wire.FSChange{Path: ch.Path, Added: add, Removed: del, InScope: true}})
			}
			return out
		}
	case "turn/completed":
		var n struct {
			Turn struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"turn"`
		}
		if json.Unmarshal(raw, &n) != nil {
			return nil
		}
		p.mu.Lock()
		text := p.lastText
		p.turnID, p.lastText = "", ""
		p.seenErr = map[string]bool{}
		p.mu.Unlock()
		if n.Turn.Status == "completed" {
			return []Event{{Type: wire.EvClaim, TS: now, Data: wire.Claim{Kind: "done", Text: truncate(text, 4000)}}}
		}
		msg := n.Turn.Status
		if n.Turn.Error != nil && n.Turn.Error.Message != "" {
			msg = n.Turn.Error.Message
		}
		return []Event{{Type: wire.EvClaim, TS: now, Data: wire.Claim{Kind: "error", Text: truncate(msg, 2000)}}}
	case "error":
		var n struct {
			Error struct {
				Message           string `json:"message"`
				AdditionalDetails string `json:"additionalDetails"`
			} `json:"error"`
			WillRetry bool `json:"willRetry"`
		}
		if json.Unmarshal(raw, &n) != nil {
			return nil
		}
		msg := n.Error.Message
		if n.Error.AdditionalDetails != "" {
			msg += "\n" + n.Error.AdditionalDetails
		}
		// Retries ("Reconnecting... 3/5", "waiting for network") repeat for as long as the problem lasts.
		// Report each kind once per turn so a stuck run is visible without flooding the timeline.
		sig := ErrorSignature(n.Error.Message)
		p.mu.Lock()
		seen := p.seenErr[sig]
		p.seenErr[sig] = true
		p.mu.Unlock()
		if seen {
			return nil
		}
		if n.WillRetry {
			msg = "(retrying) " + msg
		}
		return []Event{{Type: wire.EvError, TS: now, Data: wire.ErrorSig{Signature: sig, Message: truncate(msg, 2000), Count: 1}}}
	}
	return nil
}

func diffStat(diff string) (add, del int) {
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
		case strings.HasPrefix(l, "+"):
			add++
		case strings.HasPrefix(l, "-"):
			del++
		}
	}
	return
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
