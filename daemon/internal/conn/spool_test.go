package conn

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/helm/helm/daemon/internal/wire"
)

func ev(seq int64) wire.Event {
	return wire.Event{Seq: seq, TS: time.Unix(0, 0).UTC(), Type: wire.EvClaim, Data: wire.MustData(wire.Claim{Kind: "done"})}
}

func TestFileSpoolSurvivesRestartAndPartialAck(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewFileSpool(dir)
	_ = s.Put(wire.RunEvents{RunID: "r1", Events: []wire.Event{ev(1), ev(2)}})
	_ = s.Put(wire.RunEvents{RunID: "r1", Events: []wire.Event{ev(3)}})
	_ = s.Put(wire.RunEvents{RunID: "r2", Events: []wire.Event{ev(1)}})

	// "Restart": a new spool over the same directory sees everything.
	s2, _ := NewFileSpool(dir)
	seqs := map[string][]int64{}
	for _, b := range s2.Pending() {
		for _, e := range b.Events {
			seqs[b.RunID] = append(seqs[b.RunID], e.Seq)
		}
	}
	if len(seqs["r1"]) != 3 || len(seqs["r2"]) != 1 {
		t.Fatalf("pending after restart: %v", seqs)
	}

	s2.Done("r1", 2)
	s2.Done("r2", 1)
	p := s2.Pending()
	if len(p) != 1 || p[0].RunID != "r1" || len(p[0].Events) != 1 || p[0].Events[0].Seq != 3 {
		t.Fatalf("after acks: %+v", p)
	}
	if _, err := os.Stat(filepath.Join(dir, "r2.jsonl")); !os.IsNotExist(err) {
		t.Fatal("fully acked run file should be removed")
	}

	// A torn last line (crash mid-write) is skipped, not fatal.
	f, _ := os.OpenFile(filepath.Join(dir, "r1.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"seq":4,"ts":"20`)
	_ = f.Close()
	if p := s2.Pending(); len(p) != 1 || len(p[0].Events) != 1 {
		t.Fatalf("torn line handling: %+v", p)
	}
}
