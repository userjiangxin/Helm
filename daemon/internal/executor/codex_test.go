package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helm/helm/daemon/internal/policy"
	"github.com/helm/helm/daemon/internal/wire"
)

// TestMain lets the test binary impersonate `codex app-server --stdio` (FAKE_CODEX=1).
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_CODEX") == "1" {
		fakeAppServer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeAppServer speaks the subset of the app-server protocol helmd uses. Each turn tries to
// patch the file named in the prompt, asks for approval, then completes.
func fakeAppServer() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 1<<20), 1<<24)
	out := json.NewEncoder(os.Stdout)
	turn := 0
	reqID := 100
	for in.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
		}
		_ = json.Unmarshal(in.Bytes(), &m)
		switch m.Method {
		case "initialize":
			_ = out.Encode(map[string]any{"id": m.ID, "result": map[string]any{"userAgent": "fake"}})
		case "thread/start":
			_ = out.Encode(map[string]any{"id": m.ID, "result": map[string]any{"thread": map[string]any{"id": "th_1"}}})
		case "turn/start":
			turn++
			var p struct {
				Input []struct {
					Text string `json:"text"`
				} `json:"input"`
				SandboxPolicy map[string]any `json:"sandboxPolicy"`
			}
			_ = json.Unmarshal(m.Params, &p)
			_ = out.Encode(map[string]any{"id": m.ID, "result": map[string]any{"turn": map[string]any{"id": fmt.Sprintf("tu_%d", turn)}}})
			target := "/outside/report.md"
			if strings.Contains(p.Input[0].Text, "inside") {
				target = "/work/notes.md"
			}
			item := fmt.Sprintf("it_%d", turn)
			_ = out.Encode(map[string]any{"method": "item/started", "params": map[string]any{"item": map[string]any{
				"type": "fileChange", "id": item, "status": "inProgress", "changes": []any{map[string]any{"path": target, "kind": "add", "diff": "+hello\n"}}}}})
			reqID++
			_ = out.Encode(map[string]any{"id": reqID, "method": "item/fileChange/requestApproval", "params": map[string]any{"itemId": item, "threadId": "th_1", "turnId": "x"}})
			// wait for the approval answer
			var decision string
			for in.Scan() {
				var r struct {
					ID     json.RawMessage `json:"id"`
					Result struct {
						Decision string `json:"decision"`
					} `json:"result"`
				}
				_ = json.Unmarshal(in.Bytes(), &r)
				if string(r.ID) == fmt.Sprint(reqID) {
					decision = r.Result.Decision
					break
				}
			}
			status := "declined"
			if decision == "accept" {
				status = "completed"
			}
			_ = out.Encode(map[string]any{"method": "item/completed", "params": map[string]any{"item": map[string]any{
				"type": "fileChange", "id": item, "status": status, "changes": []any{map[string]any{"path": target, "kind": "add", "diff": "+hello\n"}}}}})
			_ = out.Encode(map[string]any{"method": "item/completed", "params": map[string]any{"item": map[string]any{
				"type": "commandExecution", "id": "c1", "command": "pytest -q", "exitCode": 1, "durationMs": 120, "aggregatedOutput": "E   KeyError: 'label'\n1 failed"}}})
			_ = out.Encode(map[string]any{"method": "item/completed", "params": map[string]any{"item": map[string]any{
				"type": "agentMessage", "id": "m1", "text": "write to " + target + " was " + status}}})
			_ = out.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "th_1", "turn": map[string]any{"id": fmt.Sprintf("tu_%d", turn), "status": "completed"}}})
		}
	}
}

func TestCodexAgainstFakeAppServer(t *testing.T) {
	self, _ := os.Executable()
	// The executor runs `<bin> app-server --stdio`; the test binary ignores those args under FAKE_CODEX.
	c := NewCodex(self)
	eng := policy.New("/work", wire.Scope{})
	var mu sync.Mutex
	var asked []policy.Request
	decide := func(r policy.Request) policy.Decision {
		mu.Lock()
		asked = append(asked, r)
		mu.Unlock()
		return eng.Decide(r)
	}
	ctx := context.Background()
	sid, err := c.Start(ctx, RunSpec{RunID: "run_c", Workdir: t.TempDir(), Decide: decide, Env: []string{"FAKE_CODEX=1"},
		Contract: wire.Contract{Intent: "write report", Prompt: "write the report"}})
	if err != nil {
		t.Fatal(err)
	}
	if c.NativeRef(sid) != "th_1" {
		t.Fatalf("thread id: %q", c.NativeRef(sid))
	}
	collect := func() []Event {
		var evs []Event
		for {
			select {
			case se := <-c.Events():
				if se.Done {
					t.Fatalf("app-server exited: %v", se.Err)
				}
				evs = append(evs, se.Event)
				if se.Event.Type == wire.EvClaim {
					return evs
				}
			case <-time.After(5 * time.Second):
				t.Fatal("timeout")
			}
		}
	}

	// Turn 1: out-of-scope write → policy hold → decline; no fs_change; failing command → cmd_exit + error.
	evs := collect()
	types := map[wire.EventType]int{}
	for _, e := range evs {
		types[e.Type]++
	}
	if types[wire.EvFSChange] != 0 || types[wire.EvCmdExit] != 1 || types[wire.EvError] != 1 || types[wire.EvAgentMessage] != 1 {
		t.Fatalf("turn 1 events: %+v", types)
	}
	if last := evs[len(evs)-1].Data.(wire.Claim); last.Kind != "done" || !strings.Contains(last.Text, "declined") {
		t.Fatalf("claim: %+v", last)
	}
	mu.Lock()
	if len(asked) != 1 || asked[0].Paths[0] != "/outside/report.md" {
		t.Fatalf("policy asked: %+v", asked)
	}
	mu.Unlock()

	// Turn 2 (idle → new turn): in-scope write is accepted and reported.
	newTurn, err := c.Send(ctx, sid, "now write inside")
	if err != nil || !newTurn {
		t.Fatalf("send: %v %v", newTurn, err)
	}
	evs = collect()
	var fs *wire.FSChange
	for _, e := range evs {
		if f, ok := e.Data.(wire.FSChange); ok {
			fs = &f
		}
	}
	if fs == nil || fs.Path != "/work/notes.md" || fs.Added != 1 {
		t.Fatalf("expected fs_change for /work/notes.md, got %+v", evs)
	}
	if err := c.Stop(ctx, sid, 2*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestWritableRoots(t *testing.T) {
	got := writableRoots("/w", []string{"/data/a/**", "/exp/*", "/x/*/y/**", "/plain"})
	want := []string{"/w", "/data/a", "/exp", "/plain"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
