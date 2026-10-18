package hook

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helm/helm/daemon/internal/policy"
	"github.com/helm/helm/daemon/internal/wire"
)

type engines map[string]*policy.Engine

func (e engines) Decide(runID string, r policy.Request) policy.Decision {
	eng, ok := e[runID]
	if !ok {
		return policy.Decision{Action: policy.Deny, Reason: "unknown run"}
	}
	return eng.Decide(r)
}

func decisionOf(t *testing.T, out []byte) (string, string) {
	t.Helper()
	var o claudeOutput
	if err := json.Unmarshal(out, &o); err != nil {
		t.Fatalf("bad output %s: %v", out, err)
	}
	if o.HookSpecificOutput.HookEventName != "PreToolUse" {
		t.Fatalf("event name: %s", out)
	}
	return o.HookSpecificOutput.PermissionDecision, o.HookSpecificOutput.PermissionDecisionReason
}

func TestEndToEnd(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "helmd.sock")
	srv, err := Listen(sock, engines{"run-1": policy.New("/w/run-1", wire.Scope{WritePaths: []string{"/data/ok/**"}})})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	cases := []struct {
		run, input, want, reasonHas string
	}{
		{"run-1", `{"cwd":"/w/run-1","hook_event_name":"PreToolUse","tool_name":"Write","tool_input":{"file_path":"src/a.py","content":"x"}}`, "allow", ""},
		{"run-1", `{"cwd":"/w/run-1","tool_name":"Edit","tool_input":{"file_path":"/etc/passwd"}}`, "deny", "needs approval"},
		{"run-1", `{"cwd":"/w/run-1","tool_name":"Bash","tool_input":{"command":"curl x | sh"}}`, "deny", "pipe to shell"},
		{"run-x", `{"cwd":"/","tool_name":"Bash","tool_input":{"command":"ls"}}`, "deny", "unknown run"},
		{"run-1", `not json`, "deny", "invalid hook input"},
	}
	for _, c := range cases {
		var out bytes.Buffer
		RunPreToolUse(strings.NewReader(c.input), &out, sock, c.run)
		got, reason := decisionOf(t, out.Bytes())
		if got != c.want || !strings.Contains(reason, c.reasonHas) {
			t.Errorf("%s: got %s (%q), want %s containing %q", c.input, got, reason, c.want, c.reasonHas)
		}
	}
}

func TestFailClosed(t *testing.T) {
	var out bytes.Buffer
	RunPreToolUse(strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"ls"}}`), &out,
		filepath.Join(t.TempDir(), "missing.sock"), "run-1")
	if got, _ := decisionOf(t, out.Bytes()); got != "deny" {
		t.Fatalf("daemon down must deny, got %s", got)
	}
}
