package registry

import "testing"

func TestParseGPUList(t *testing.T) {
	out := "0, NVIDIA H800, 81559\n1, NVIDIA H800, 81559\nbad line\n"
	g := ParseGPUList(out)
	if len(g) != 2 || g[1].Index != 1 || g[0].Name != "NVIDIA H800" || g[0].MemTotalMB != 81559 {
		t.Fatalf("got %+v", g)
	}
	if ParseGPUList("") != nil {
		t.Fatal("empty output should yield no GPUs")
	}
}

func TestParseLoad(t *testing.T) {
	idle, total, ok := ParseProcStat("cpu  100 0 50 800 50 0 0 0 0 0\ncpu0 1 2 3 4\n")
	if !ok || idle != 850 || total != 1000 {
		t.Fatalf("idle=%d total=%d ok=%v", idle, total, ok)
	}
	g := ParseGPULoad("0, 87, 30210, 81559\n1, 0, 3, 81559\n")
	if len(g) != 2 || g[0].UtilPct != 87 || g[0].MemUsedMB != 30210 || g[1].Index != 1 {
		t.Fatalf("%+v", g)
	}
	s := NewLoadSampler()
	if l := s.Sample(t.Context()); l.CPUPct < 0 || l.CPUPct > 100 {
		t.Fatalf("cpu %v", l.CPUPct)
	}
}
