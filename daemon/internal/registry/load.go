package registry

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/helm/helm/daemon/internal/wire"
)

// LoadSampler reports CPU utilisation (from /proc/stat deltas) and per-GPU utilisation/memory
// (nvidia-smi). Sampled lazily: each Sample call measures since the previous call.
type LoadSampler struct {
	mu        sync.Mutex
	prevIdle  uint64
	prevTotal uint64
	hasGPU    bool
}

func NewLoadSampler() *LoadSampler {
	_, err := exec.LookPath("nvidia-smi")
	s := &LoadSampler{hasGPU: err == nil}
	s.cpu() // prime the delta
	return s
}

func (s *LoadSampler) Sample(ctx context.Context) wire.Load {
	l := wire.Load{CPUPct: s.cpu()}
	if s.hasGPU {
		l.GPUs = ParseGPULoad(run(ctx, "nvidia-smi",
			"--query-gpu=index,utilization.gpu,memory.used,memory.total", "--format=csv,noheader,nounits"))
	}
	return l
}

func (s *LoadSampler) cpu() float64 {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0 // macOS: not reported in M0
	}
	idle, total, ok := ParseProcStat(string(b))
	if !ok {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dIdle, dTotal := idle-s.prevIdle, total-s.prevTotal
	s.prevIdle, s.prevTotal = idle, total
	if dTotal == 0 || dTotal == total { // first sample has no previous reading
		return 0
	}
	return float64(int(1000*(1-float64(dIdle)/float64(dTotal)))) / 10
}

// ParseProcStat returns idle (idle+iowait) and total jiffies from the aggregate "cpu" line.
func ParseProcStat(stat string) (idle, total uint64, ok bool) {
	for _, line := range strings.Split(stat, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || f[0] != "cpu" {
			continue
		}
		for i, v := range f[1:] {
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return 0, 0, false
			}
			total += n
			if i == 3 || i == 4 { // idle, iowait
				idle += n
			}
		}
		return idle, total, true
	}
	return 0, 0, false
}

// ParseGPULoad parses `index, util, mem.used, mem.total` CSV rows.
func ParseGPULoad(out string) []wire.GPULoad {
	var gs []wire.GPULoad
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, ",")
		if len(f) != 4 {
			continue
		}
		var nums [4]float64
		okAll := true
		for i := range f {
			v, err := strconv.ParseFloat(strings.TrimSpace(f[i]), 64)
			if err != nil {
				okAll = false
				break
			}
			nums[i] = v
		}
		if okAll {
			gs = append(gs, wire.GPULoad{Index: int(nums[0]), UtilPct: nums[1], MemUsedMB: int(nums[2]), MemTotalMB: int(nums[3])})
		}
	}
	return gs
}
