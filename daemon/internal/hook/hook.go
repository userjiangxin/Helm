// Package hook bridges Claude Code's PreToolUse hook to the daemon's policy engine.
//
//	claude ──(PreToolUse JSON on stdin)──> `helmd hook pre-tool-use --run <id>`
//	        ──(unix socket)──> helmd Server ──> policy.Engine.Decide
//	        <──(decision JSON on stdout)──
//
// The hook process is fail-closed: if the daemon cannot be reached, the tool call is denied.
// A Hold decision is reported to Claude as "deny" with a waiting message (hooks have
// timeouts, so we never block for a human); the daemon moves the Run to GATED and,
// once approved, injects a CONTINUE message on the next turn.
package hook

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/helm/helm/daemon/internal/policy"
)

// ---------- Claude Code hook I/O ----------

type ClaudeInput struct {
	SessionID     string          `json:"session_id"`
	Cwd           string          `json:"cwd"`
	HookEventName string          `json:"hook_event_name"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
}

type claudeOutput struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	} `json:"hookSpecificOutput"`
}

// ToRequest converts a Claude tool call into the agent-neutral policy request.
func ToRequest(in ClaudeInput) policy.Request {
	var ti struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
		Command      string `json:"command"`
	}
	_ = json.Unmarshal(in.ToolInput, &ti)
	r := policy.Request{Tool: in.ToolName, Command: ti.Command}
	for _, p := range []string{ti.FilePath, ti.NotebookPath} {
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) && in.Cwd != "" {
			p = filepath.Join(in.Cwd, p)
		}
		r.Paths = append(r.Paths, p)
	}
	return r
}

// RenderClaude turns a policy decision into the PreToolUse output JSON.
func RenderClaude(d policy.Decision) []byte {
	var o claudeOutput
	o.HookSpecificOutput.HookEventName = "PreToolUse"
	switch d.Action {
	case policy.Allow:
		o.HookSpecificOutput.PermissionDecision = "allow"
	case policy.Hold:
		o.HookSpecificOutput.PermissionDecision = "deny"
		o.HookSpecificOutput.PermissionDecisionReason = "Helm: this action needs approval (" + d.Reason +
			"). It has been sent for review. Do not retry it or work around it; continue with other parts of the task or wait for instructions."
	default:
		o.HookSpecificOutput.PermissionDecision = "deny"
		o.HookSpecificOutput.PermissionDecisionReason = "Helm: " + d.Reason
	}
	b, _ := json.Marshal(o)
	return b
}

// ---------- socket protocol ----------

type sockRequest struct {
	RunID   string         `json:"run_id"`
	Request policy.Request `json:"request"`
}

// Decider is implemented by the run supervisor: it owns one policy.Engine per run
// and is told about Hold decisions so it can move the run to GATED.
type Decider interface {
	Decide(runID string, r policy.Request) policy.Decision
}

type Server struct {
	path string
	ln   net.Listener
	wg   sync.WaitGroup
}

func Listen(path string, d Decider) (*Server, error) {
	_ = os.Remove(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	s := &Server{path: path, ln: ln}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serve(c, d)
		}
	}()
	return s, nil
}

func (s *Server) Close() error {
	err := s.ln.Close()
	s.wg.Wait()
	_ = os.Remove(s.path)
	return err
}

func serve(c net.Conn, d Decider) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	var req sockRequest
	if err := json.NewDecoder(c).Decode(&req); err != nil {
		return
	}
	_ = json.NewEncoder(c).Encode(d.Decide(req.RunID, req.Request))
}

// Ask is used by the `helmd hook` subcommand.
func Ask(sockPath, runID string, r policy.Request) (policy.Decision, error) {
	c, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return policy.Decision{}, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(8 * time.Second))
	if err := json.NewEncoder(c).Encode(sockRequest{RunID: runID, Request: r}); err != nil {
		return policy.Decision{}, err
	}
	var d policy.Decision
	if err := json.NewDecoder(bufio.NewReader(c)).Decode(&d); err != nil {
		return policy.Decision{}, err
	}
	if d.Action == "" {
		return d, errors.New("empty decision")
	}
	return d, nil
}

// RunPreToolUse is the whole `helmd hook pre-tool-use` program: stdin -> stdout, fail-closed.
func RunPreToolUse(stdin io.Reader, stdout io.Writer, sockPath, runID string) {
	var in ClaudeInput
	if err := json.NewDecoder(stdin).Decode(&in); err != nil {
		_, _ = stdout.Write(RenderClaude(policy.Decision{Action: policy.Deny, Reason: "invalid hook input"}))
		return
	}
	d, err := Ask(sockPath, runID, ToRequest(in))
	if err != nil {
		d = policy.Decision{Action: policy.Deny, Reason: "Helm daemon unreachable; refusing tool call (fail-closed)"}
	}
	_, _ = stdout.Write(RenderClaude(d))
}
