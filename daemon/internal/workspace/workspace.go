// Package workspace prepares the directory a Run works in.
//
//   - contract.repo set:  one shared bare clone per repository (~/.helm/repos/<hash>.git) and a
//     fresh detached git worktree per run (~/.helm/work/<run>), checked out at base_ref.
//     The starting commit is recorded as the snapshot for ROLLBACK.
//   - contract.workdir set: that existing directory is used as is (typical for data/training work).
//   - neither: an empty scratch directory per run.
package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/helm/helm/daemon/internal/wire"
)

type Manager struct {
	Root string // e.g. ~/.helm  (repos/ and work/ live under it)

	mu    sync.Mutex
	locks map[string]*sync.Mutex // per-repo lock: git does not like concurrent fetches into one repo
}

type Workspace struct {
	Dir      string
	Snapshot string // commit at start (git workspaces only)
	Managed  bool   // created by Helm, safe to delete later
}

func New(root string) *Manager { return &Manager{Root: root, locks: map[string]*sync.Mutex{}} }

func (m *Manager) repoLock(key string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.locks[key]
	if !ok {
		l = &sync.Mutex{}
		m.locks[key] = l
	}
	return l
}

func (m *Manager) Prepare(ctx context.Context, runID string, c wire.Contract) (Workspace, error) {
	switch {
	case c.Repo != "":
		return m.worktree(ctx, runID, c.Repo, c.BaseRef)
	case c.Workdir != "":
		dir := expandHome(c.Workdir)
		st, err := os.Stat(dir)
		if err != nil || !st.IsDir() {
			return Workspace{}, fmt.Errorf("workdir %s does not exist on this machine", c.Workdir)
		}
		return Workspace{Dir: dir}, nil
	default:
		dir := filepath.Join(m.Root, "work", runID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return Workspace{}, err
		}
		return Workspace{Dir: dir, Managed: true}, nil
	}
}

func (m *Manager) worktree(ctx context.Context, runID, repo, ref string) (Workspace, error) {
	if ref == "" {
		ref = "HEAD"
	}
	sum := sha256.Sum256([]byte(repo))
	key := hex.EncodeToString(sum[:8])
	bare := filepath.Join(m.Root, "repos", key+".git")
	dir := filepath.Join(m.Root, "work", runID)

	l := m.repoLock(key)
	l.Lock()
	defer l.Unlock()

	if _, err := os.Stat(bare); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
			return Workspace{}, err
		}
		if _, err := git(ctx, "", "clone", "--bare", "--quiet", repo, bare); err != nil {
			return Workspace{}, err
		}
		// Bare clones do not track remote branches by default; make `fetch` update refs/heads/*.
		if _, err := git(ctx, bare, "config", "remote.origin.fetch", "+refs/heads/*:refs/heads/*"); err != nil {
			return Workspace{}, err
		}
	} else if _, err := git(ctx, bare, "fetch", "--quiet", "--prune", "origin"); err != nil {
		return Workspace{}, err
	}
	_, _ = git(ctx, bare, "worktree", "prune")
	if _, err := git(ctx, bare, "worktree", "add", "--detach", "--quiet", dir, ref); err != nil {
		return Workspace{}, err
	}
	head, err := git(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return Workspace{}, err
	}
	return Workspace{Dir: dir, Snapshot: strings.TrimSpace(head), Managed: true}, nil
}

// Rollback resets a git workspace to its snapshot, discarding all changes and untracked files.
func (m *Manager) Rollback(ctx context.Context, w Workspace) error {
	if w.Snapshot == "" {
		return errors.New("rollback needs a git workspace (contract.repo)")
	}
	if _, err := git(ctx, w.Dir, "reset", "--hard", "--quiet", w.Snapshot); err != nil {
		return err
	}
	_, err := git(ctx, w.Dir, "clean", "-fdq")
	return err
}

// Cleanup removes Helm-managed run directories under work/ not modified for olderThan,
// except those listed in keep (runs still active).
func (m *Manager) Cleanup(ctx context.Context, olderThan time.Duration, keep map[string]bool) []string {
	workRoot := filepath.Join(m.Root, "work")
	entries, _ := os.ReadDir(workRoot)
	var removed []string
	for _, e := range entries {
		if !e.IsDir() || keep[e.Name()] {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < olderThan {
			continue
		}
		if err := os.RemoveAll(filepath.Join(workRoot, e.Name())); err == nil {
			removed = append(removed, e.Name())
		}
	}
	// Forget worktree metadata whose directories are gone.
	repos, _ := filepath.Glob(filepath.Join(m.Root, "repos", "*.git"))
	for _, r := range repos {
		_, _ = git(ctx, r, "worktree", "prune")
	}
	return removed
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}
