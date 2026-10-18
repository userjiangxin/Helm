package conn

import (
	"sync"

	"github.com/helm/helm/daemon/internal/wire"
)

// Spool buffers event batches until the control plane has them.
// M0 ships the in-memory version; TODO(M0-D3): a file-backed spool (append-only
// JSONL per run under ~/.helm/spool) so events survive a daemon restart.
type Spool interface {
	Put(b wire.RunEvents)
	Pending() []wire.RunEvents
	Done(runID string, upToSeq int64)
}

type MemSpool struct {
	mu    sync.Mutex
	items []wire.RunEvents
}

func (s *MemSpool) Put(b wire.RunEvents) {
	if len(b.Events) == 0 {
		return
	}
	s.mu.Lock()
	s.items = append(s.items, b)
	s.mu.Unlock()
}

func (s *MemSpool) Pending() []wire.RunEvents {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]wire.RunEvents(nil), s.items...)
}

func (s *MemSpool) Done(runID string, upToSeq int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.items[:0]
	for _, b := range s.items {
		if b.RunID == runID && b.Events[len(b.Events)-1].Seq <= upToSeq {
			continue
		}
		kept = append(kept, b)
	}
	s.items = kept
}
