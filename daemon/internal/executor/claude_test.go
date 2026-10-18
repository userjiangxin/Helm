package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helm/helm/daemon/internal/wire"
)

func TestErrorSignature(t *testing.T) {
	a := ErrorSignature("Traceback ...\nKeyError: 'label' at /data/x/part-0001.parquet line 42")
	b := ErrorSignature("KeyError: 'label_v2' at /data/y/part-0099.parquet line 7")
	c := ErrorSignature("ValueError: bad shape (3, 4)")
	if a != b || a == c || len(a) != 8 {
		t.Fatalf("a=%s b=%s c=%s", a, b, c)
	}
}

func TestNormalizeClaudeLine(t *testing.T) {
	cases := []struct {
		line string
		want []wire.EventType
	}{
		{`{"type":"system","subtype":"init","session_id":"s"}`, nil},
		{`{"type":"assistant","message":{"content":[{"type":"text","text":"working on it"},{"type":"tool_use","name":"Write"}]}}`, []wire.EventType{wire.EvAgentMessage}},
		{`{"type":"user","message":{"content":[{"type":"tool_result","is_error":true,"content":"PreToolUse:Write hook error: Helm: blocked"}]}}`, nil},
		{`{"type":"user","message":{"content":[{"type":"tool_result","is_error":true,"content":[{"type":"text","text":"Exit code 1\nModuleNotFoundError: No module named 'torch'"}]}]}}`, []wire.EventType{wire.EvError}},
		{`{"type":"result","subtype":"success","is_error":false,"result":"All done."}`, []wire.EventType{wire.EvClaim}},
		{`not json`, nil},
	}
	for _, c := range cases {
		got := NormalizeClaudeLine([]byte(c.line))
		if len(got) != len(c.want) {
			t.Fatalf("%s: got %d events", c.line, len(got))
		}
		for i := range got {
			if got[i].Type != c.want[i] {
				t.Fatalf("%s: got %s", c.line, got[i].Type)
			}
		}
	}
}

func TestInitialPromptMentionsScopeAndAcceptance(t *testing.T) {
	p := InitialPrompt(wire.Contract{Intent: "prep", Prompt: "do x", Acceptance: []string{"rows > 10"},
		Scope: wire.Scope{WritePaths: []string{"/data/a/**"}, ReadonlyPaths: []string{"/data/prod/**"}}})
	for _, want := range []string{"prep", "do x", "rows > 10", "/data/a/**", "/data/prod/**"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt missing %q:\n%s", want, p)
		}
	}
}

// fakeClaude is a shell stand-in for `claude`: it records its argv, echoes a turn per input line,
// and exits when stdin closes. Lets us test process plumbing without the real CLI.
const fakeClaude = `#!/bin/sh
echo "$@" > "$FAKE_ARGS"
echo '{"type":"system","subtype":"init","session_id":"x"}'
while IFS= read -r line; do
  echo '{"type":"assistant","message":{"content":[{"type":"text","text":"got it"}]}}'
  echo '{"type":"result","subtype":"success","is_error":false,"result":"turn done"}'
done
`

func TestClaudeProcessPlumbing(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(fakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	args := filepath.Join(dir, "args")
	c := NewClaude(bin, "/usr/local/bin/helmd", filepath.Join(dir, "state"))
	ctx := context.Background()
	sid, err := c.Start(ctx, RunSpec{RunID: "run_1", Workdir: dir, HookSock: "/tmp/h.sock",
		Contract: wire.Contract{Intent: "i", Prompt: "p"}, Env: []string{"FAKE_ARGS=" + args}})
	if err != nil {
		t.Fatal(err)
	}
	claims := 0
	next := func() SessionEvent {
		select {
		case e := <-c.Events():
			return e
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for event")
		}
		return SessionEvent{}
	}
	for claims < 1 {
		if e := next(); e.Event.Type == wire.EvClaim {
			claims++
		}
	}
	if _, err := c.Send(ctx, sid, "please continue"); err != nil {
		t.Fatal(err)
	}
	for claims < 2 {
		if e := next(); e.Event.Type == wire.EvClaim {
			claims++
		}
	}
	if c.NativeRef(sid) == "" {
		t.Fatal("expected a native session id")
	}
	if err := c.Stop(ctx, sid, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	for e := next(); !e.Done; e = next() {
	}

	argv, _ := os.ReadFile(args)
	for _, want := range []string{"--input-format stream-json", "--output-format stream-json", "--session-id", "--settings", "--permission-mode bypassPermissions"} {
		if !strings.Contains(string(argv), want) {
			t.Fatalf("argv missing %q: %s", want, argv)
		}
	}
	var settings struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	b, _ := os.ReadFile(filepath.Join(dir, "state", "run_1", "claude-settings.json"))
	_ = json.Unmarshal(b, &settings)
	h := settings.Hooks["PreToolUse"]
	if len(h) != 1 || h[0].Matcher != "*" || !strings.Contains(h[0].Hooks[0].Command, "hook pre-tool-use --run run_1 --sock /tmp/h.sock") {
		t.Fatalf("bad settings: %s", b)
	}
}
