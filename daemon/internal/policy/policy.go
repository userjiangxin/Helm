// Package policy is the reflex layer: it decides in-process, in microseconds,
// whether a tool call proposed by an agent may run. Prompts only persuade the
// agent; this package enforces the Contract scope.
package policy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/helm/helm/daemon/internal/wire"
)

type Action string

const (
	Allow Action = "allow"
	Deny  Action = "deny"
	Hold  Action = "hold" // needs brain / human approval -> Run goes GATED
)

type Decision struct {
	Action Action `json:"action"`
	Reason string `json:"reason,omitempty"`
}

// Request is the agent-neutral form of a proposed tool call.
type Request struct {
	Tool    string // e.g. Write, Edit, Bash (Claude) or apply_patch / exec (Codex)
	Paths   []string
	Command string
}

// Global rules applied to every run regardless of contract.
var globalHoldPatterns = []struct {
	re   *regexp.Regexp
	name string
}{
	{regexp.MustCompile(`\brm\s+-[a-zA-Z]*(rf|fr)[a-zA-Z]*\b`), "rm -rf"},
	{regexp.MustCompile(`\bgit\s+push\b.*(--force\b|\s-f\b)`), "git push --force"},
	{regexp.MustCompile(`\|\s*(sudo\s+)?(sh|bash|zsh)\b`), "pipe to shell"},
	{regexp.MustCompile(`\bmkfs(\.\w+)?\b`), "mkfs"},
	{regexp.MustCompile(`\bdd\s+if=`), "dd"},
}

type Engine struct {
	mu       sync.RWMutex
	root     string // run worktree / workdir; always writable
	write    []*regexp.Regexp
	readonly []*regexp.Regexp
	holdCmds []string
	paused   bool
}

func New(root string, s wire.Scope) *Engine {
	e := &Engine{root: clean(root)}
	e.Apply(s)
	return e
}

// Apply replaces the scope (used at start and on AMEND directives).
func (e *Engine) Apply(s wire.Scope) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.write = compileAll(s.WritePaths)
	e.readonly = compileAll(s.ReadonlyPaths)
	e.holdCmds = append([]string{}, s.DenyCommands...)
}

// SetPaused makes every subsequent tool call deny until resumed (PAUSE directive).
func (e *Engine) SetPaused(p bool) {
	e.mu.Lock()
	e.paused = p
	e.mu.Unlock()
}

func (e *Engine) Decide(r Request) Decision {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.paused {
		return Decision{Deny, "run is paused by Helm; wait for further instructions"}
	}
	for _, p := range r.Paths {
		p = clean(p)
		if matchAny(e.readonly, p) {
			return Decision{Deny, "path is read-only in this contract: " + p}
		}
		if within(e.root, p) || matchAny(e.write, p) {
			continue
		}
		return Decision{Hold, "write outside contract scope: " + p}
	}
	if cmd := normalizeCmd(r.Command); cmd != "" {
		for _, g := range globalHoldPatterns {
			if g.re.MatchString(cmd) {
				return Decision{Hold, "high-risk command needs approval: " + g.name}
			}
		}
		for _, bad := range e.holdCmds {
			if strings.Contains(cmd, normalizeCmd(bad)) {
				return Decision{Hold, "command denied by contract, needs approval: " + bad}
			}
		}
	}
	return Decision{Allow, ""}
}

// ---------- helpers ----------

func clean(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	}
	return filepath.Clean(p)
}

func within(root, p string) bool {
	if root == "" || root == "." {
		return false
	}
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// globToRegexp supports ** (any depth), * (one segment) and ?.
func globToRegexp(g string) *regexp.Regexp {
	g = clean(g)
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(g); i++ {
		switch c := g[i]; c {
		case '*':
			if i+1 < len(g) && g[i+1] == '*' {
				i++
				if i+1 < len(g) && g[i+1] == '/' { // "**/" matches zero or more dirs
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

func compileAll(globs []string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(globs))
	for _, g := range globs {
		out = append(out, globToRegexp(g))
	}
	return out
}

func matchAny(rs []*regexp.Regexp, p string) bool {
	for _, r := range rs {
		if r.MatchString(p) {
			return true
		}
	}
	return false
}

var spaces = regexp.MustCompile(`\s+`)

func normalizeCmd(c string) string {
	return spaces.ReplaceAllString(strings.TrimSpace(c), " ")
}
