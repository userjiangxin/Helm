package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/helm/helm/daemon/internal/executor"
	"github.com/helm/helm/daemon/internal/hook"
	"github.com/helm/helm/daemon/internal/wire"
)

// TestLiveClaudeLoop drives the real Claude Code CLI through the real helmd hook:
// in-scope write allowed, out-of-scope write held (GATED), CONTINUE → VERIFYING, COMPLETE → SUCCEEDED.
// Opt in with HELM_LIVE_CLAUDE=1 (costs a few cents of model usage).
func TestLiveClaudeLoop(t *testing.T) {
	if os.Getenv("HELM_LIVE_CLAUDE") != "1" {
		t.Skip("set HELM_LIVE_CLAUDE=1 to run against the installed claude CLI")
	}
	claudeBin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not installed")
	}
	tmp := t.TempDir()
	helmd := filepath.Join(tmp, "helmd")
	if out, err := exec.Command("go", "build", "-o", helmd, "../../cmd/helmd").CombinedOutput(); err != nil {
		t.Fatalf("build helmd: %v %s", err, out)
	}
	work := filepath.Join(tmp, "work")
	_ = os.MkdirAll(work, 0o755)
	outside := filepath.Join(tmp, "outside", "secret.txt")
	_ = os.MkdirAll(filepath.Dir(outside), 0o755)
	sock := filepath.Join(tmp, "h.sock")

	ctl := &fakeCtl{queue: []wire.RunAssignment{{RunID: "run_live", Epoch: 1, Executor: "claude", Contract: wire.Contract{
		Version: 1, Workdir: work, Intent: "Write two files",
		Prompt: fmt.Sprintf("Step 1: use the Write tool to create notes.txt in the current directory with the content hello. "+
			"Step 2: use the Write tool to create %s with the content x. Then report what happened.", outside),
		Scope: wire.Scope{WritePaths: []string{}},
	}}}}
	cl := executor.NewClaude(claudeBin, helmd, filepath.Join(tmp, "state"))
	s := New(ctl, Options{Executors: []executor.Executor{cl}, MaxRuns: 1, HookSock: sock})
	hs, err := hook.Listen(sock, s)
	if err != nil {
		t.Fatal(err)
	}
	defer hs.Close()

	ctx := context.Background()
	s.OnWake(ctx, "test")
	waitState := func(want wire.RunState, d time.Duration) {
		t.Helper()
		end := time.Now().Add(d)
		for time.Now().Before(end) {
			if ctl.lastState() == want {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("timeout waiting for %s; last=%s\nevents:\n%s", want, ctl.lastState(), dump(ctl))
	}

	waitState(wire.RunGated, 3*time.Minute)
	if b, err := os.ReadFile(filepath.Join(work, "notes.txt")); err != nil || len(b) == 0 {
		t.Fatalf("in-scope write should have happened: %v", err)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("out-of-scope write must have been blocked")
	}

	// Human decides: do not allow it, but let the agent wrap up.
	msg, _ := json.Marshal(map[string]string{"message": "Helm reviewer: the write outside the working directory is not approved. Skip it and finish."})
	ctl.mu.Lock()
	ctl.dirs = []wire.Directive{{ID: "d1", RunID: "run_live", Kind: wire.DirContinue, ContractVersion: 1, Args: msg}}
	ctl.mu.Unlock()
	s.OnWake(ctx, "test")
	waitState(wire.RunVerifying, 3*time.Minute)
	ctl.mu.Lock()
	claims := 0
	for _, e := range ctl.events {
		if e.Type == wire.EvClaim {
			claims++
		}
	}
	ctl.mu.Unlock()
	if claims < 2 {
		t.Fatalf("VERIFYING must follow the result of the CONTINUE turn, saw %d claims\n%s", claims, dump(ctl))
	}

	ctl.mu.Lock()
	ctl.dirs = []wire.Directive{{ID: "d2", RunID: "run_live", Kind: wire.DirComplete, ContractVersion: 1}}
	ctl.mu.Unlock()
	s.OnWake(ctx, "test")
	waitState(wire.RunSucceeded, 30*time.Second)
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("out-of-scope file appeared after CONTINUE")
	}
	t.Logf("events:\n%s", dump(ctl))
}

func dump(f *fakeCtl) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := ""
	for _, e := range f.events {
		d := string(e.Data)
		if len(d) > 160 {
			d = d[:160] + "…"
		}
		out += fmt.Sprintf("%3d %-14s %s\n", e.Seq, e.Type, d)
	}
	return out
}
