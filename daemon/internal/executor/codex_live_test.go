package executor

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/helm/helm/daemon/internal/policy"
	"github.com/helm/helm/daemon/internal/wire"
)

// TestCodexLive starts a real `codex app-server` and runs one turn. Opt in: HELM_LIVE_CODEX=1.
// Without a logged-in Codex the turn ends with an error claim; that still proves the request
// payloads (initialize, thread/start, turn/start with sandboxPolicy) are accepted.
func TestCodexLive(t *testing.T) {
	if os.Getenv("HELM_LIVE_CODEX") != "1" {
		t.Skip("set HELM_LIVE_CODEX=1 to run against the installed codex CLI")
	}
	bin := os.Getenv("CODEX_BIN")
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("codex"); err != nil {
			t.Skip("codex not installed")
		}
	}
	c := NewCodex(bin)
	eng := policy.New(t.TempDir(), wire.Scope{})
	ctx := context.Background()
	sid, err := c.Start(ctx, RunSpec{RunID: "run_live", Workdir: t.TempDir(), Decide: eng.Decide,
		Contract: wire.Contract{Intent: "say hi", Prompt: "Reply with the single word: hi", Scope: wire.Scope{WritePaths: []string{"/tmp/helm-live/**"}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Stop(ctx, sid, 2*time.Second)
	t.Logf("thread %s", c.NativeRef(sid))
	deadline := time.After(3 * time.Minute)
	for {
		select {
		case se := <-c.Events():
			if se.Done {
				t.Fatalf("app-server exited: %v", se.Err)
			}
			t.Logf("%s %+v", se.Event.Type, se.Event.Data)
			if cl, ok := se.Event.Data.(wire.Claim); ok {
				t.Logf("turn finished: kind=%s", cl.Kind)
				return
			}
			// Without OpenAI access Codex retries forever; seeing its error proves the turn started.
			if os.Getenv("HELM_LIVE_CODEX_OFFLINE") == "1" && se.Event.Type == wire.EvError {
				t.Log("turn started and reported a connection error (offline mode)")
				return
			}
		case <-deadline:
			t.Fatal("no turn result within 3 minutes")
		}
	}
}
