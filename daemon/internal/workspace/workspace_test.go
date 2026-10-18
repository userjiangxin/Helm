package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helm/helm/daemon/internal/wire"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestWorktreesAreIsolatedAndRollBack(t *testing.T) {
	origin := t.TempDir()
	run(t, origin, "git", "init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(origin, "a.txt"), []byte("v1\n"), 0o644)
	run(t, origin, "git", "add", ".")
	run(t, origin, "git", "commit", "-qm", "v1")

	m := New(t.TempDir())
	ctx := context.Background()
	c := wire.Contract{Repo: origin, BaseRef: "main"}
	w1, err := m.Prepare(ctx, "run_1", c)
	if err != nil {
		t.Fatal(err)
	}
	w2, err := m.Prepare(ctx, "run_2", c)
	if err != nil {
		t.Fatal(err)
	}
	if w1.Dir == w2.Dir || w1.Snapshot == "" || w1.Snapshot != w2.Snapshot {
		t.Fatalf("w1=%+v w2=%+v", w1, w2)
	}
	_ = os.WriteFile(filepath.Join(w1.Dir, "a.txt"), []byte("changed\n"), 0o644)
	_ = os.WriteFile(filepath.Join(w1.Dir, "new.txt"), []byte("x"), 0o644)
	if b, _ := os.ReadFile(filepath.Join(w2.Dir, "a.txt")); string(b) != "v1\n" {
		t.Fatal("runs must not see each other's changes")
	}

	// New upstream commit is picked up by the next run (fetch into the shared bare clone).
	_ = os.WriteFile(filepath.Join(origin, "a.txt"), []byte("v2\n"), 0o644)
	run(t, origin, "git", "commit", "-qam", "v2")
	w3, err := m.Prepare(ctx, "run_3", c)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(w3.Dir, "a.txt")); string(b) != "v2\n" {
		t.Fatalf("run_3 should start from v2, got %q", b)
	}

	if err := m.Rollback(ctx, w1); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(w1.Dir, "a.txt")); string(b) != "v1\n" {
		t.Fatal("rollback must restore the snapshot")
	}
	if _, err := os.Stat(filepath.Join(w1.Dir, "new.txt")); !os.IsNotExist(err) {
		t.Fatal("rollback must remove untracked files")
	}

	removed := m.Cleanup(ctx, -time.Second, map[string]bool{"run_3": true})
	if len(removed) != 2 {
		t.Fatalf("cleanup removed %v", removed)
	}
	if _, err := os.Stat(w3.Dir); err != nil {
		t.Fatal("kept run must survive cleanup")
	}
}

func TestWorkdirAndScratch(t *testing.T) {
	m := New(t.TempDir())
	ctx := context.Background()
	if _, err := m.Prepare(ctx, "r", wire.Contract{Workdir: "/definitely/not/here"}); err == nil {
		t.Fatal("missing workdir must be an error")
	}
	dir := t.TempDir()
	w, err := m.Prepare(ctx, "r", wire.Contract{Workdir: dir})
	if err != nil || w.Dir != dir || w.Managed {
		t.Fatalf("%+v %v", w, err)
	}
	w, err = m.Prepare(ctx, "r2", wire.Contract{})
	if err != nil || !w.Managed {
		t.Fatalf("%+v %v", w, err)
	}
}
