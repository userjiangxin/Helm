// Package registry detects what this machine can do: agent CLIs, GPUs, labels.
package registry

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/helm/helm/daemon/internal/wire"
)

// KnownAgents are the CLIs helmd knows how to drive (M0: codex, claude).
var KnownAgents = []string{"codex", "claude"}

func Detect(ctx context.Context, daemonVersion string, labels []string, maxRuns int) wire.Capabilities {
	host, _ := os.Hostname()
	c := wire.Capabilities{
		MachineName:       host,
		DaemonVersion:     daemonVersion,
		OS:                runtime.GOOS,
		Arch:              runtime.GOARCH,
		Labels:            append([]string{}, labels...),
		Agents:            []wire.AgentCLI{},
		GPUs:              DetectGPUs(ctx),
		MaxConcurrentRuns: maxRuns,
	}
	for _, name := range KnownAgents {
		if a, ok := detectAgent(ctx, name); ok {
			c.Agents = append(c.Agents, a)
		}
	}
	if c.GPUs == nil {
		c.GPUs = []wire.GPU{}
	}
	if len(c.GPUs) > 0 && !contains(c.Labels, "gpu") {
		c.Labels = append(c.Labels, "gpu")
	}
	return c
}

func detectAgent(ctx context.Context, name string) (wire.AgentCLI, bool) {
	path, err := exec.LookPath(name)
	if err != nil {
		return wire.AgentCLI{}, false
	}
	return wire.AgentCLI{Name: name, Path: path, Version: firstLine(run(ctx, path, "--version"))}, true
}

// DetectGPUs parses `nvidia-smi --query-gpu=index,name,memory.total --format=csv,noheader,nounits`.
func DetectGPUs(ctx context.Context) []wire.GPU {
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		return nil
	}
	return ParseGPUList(run(ctx, "nvidia-smi", "--query-gpu=index,name,memory.total", "--format=csv,noheader,nounits"))
}

func ParseGPUList(out string) []wire.GPU {
	var gpus []wire.GPU
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, ",")
		if len(f) != 3 {
			continue
		}
		idx, err1 := strconv.Atoi(strings.TrimSpace(f[0]))
		mem, err2 := strconv.Atoi(strings.TrimSpace(f[2]))
		if err1 != nil || err2 != nil {
			continue
		}
		gpus = append(gpus, wire.GPU{Index: idx, Name: strings.TrimSpace(f[1]), MemTotalMB: mem})
	}
	return gpus
}

func run(ctx context.Context, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
