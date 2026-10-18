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
