package conn

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/helm/helm/daemon/internal/wire"
)

// Spool buffers event batches until the control plane acknowledges them (events_ack).
type Spool interface {
	Put(b wire.RunEvents) error
	// Pending returns unacknowledged batches, oldest first, per run in seq order.
	Pending() []wire.RunEvents
	// Done drops every event of runID with seq <= upToSeq.
	Done(runID string, upToSeq int64)
}

// ---------- in-memory (tests) ----------

type MemSpool struct {
	mu    sync.Mutex
	items []wire.RunEvents
}

func (s *MemSpool) Put(b wire.RunEvents) error {
	if len(b.Events) == 0 {
		return nil
	}
	s.mu.Lock()
	s.items = append(s.items, b)
	s.mu.Unlock()
	return nil
}

func (s *MemSpool) Pending() []wire.RunEvents {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]wire.RunEvents(nil), s.items...)
}

func (s *MemSpool) Done(runID string, upToSeq int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = dropAcked(s.items, runID, upToSeq)
}

func dropAcked(items []wire.RunEvents, runID string, upTo int64) []wire.RunEvents {
	out := items[:0]
	for _, b := range items {
		if b.RunID == runID {
			kept := b.Events[:0:0]
			for _, e := range b.Events {
				if e.Seq > upTo {
					kept = append(kept, e)
				}
			}
			if len(kept) == 0 {
				continue
			}
			b.Events = kept
		}
		out = append(out, b)
	}
	return out
}

// ---------- on disk ----------

// FileSpool keeps one append-only JSONL file per run under dir. Each line is one event.
// Survives daemon restarts and crashes: Put fsyncs before returning.
type FileSpool struct {
	dir string
	mu  sync.Mutex
}

func NewFileSpool(dir string) (*FileSpool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &FileSpool{dir: dir}, nil
}

func (s *FileSpool) path(runID string) string {
	return filepath.Join(s.dir, strings.ReplaceAll(runID, "/", "_")+".jsonl")
}

func (s *FileSpool) Put(b wire.RunEvents) error {
	if len(b.Events) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.path(b.RunID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, e := range b.Events {
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		_, _ = w.Write(append(line, '\n'))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return f.Sync()
}

func (s *FileSpool) read(runID string) []wire.Event {
	f, err := os.Open(s.path(runID))
	if err != nil {
		return nil
	}
	defer f.Close()
	var evs []wire.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		var e wire.Event
		if json.Unmarshal(sc.Bytes(), &e) == nil { // a torn last line after a crash is skipped
			evs = append(evs, e)
		}
	}
	return evs
}

func (s *FileSpool) Pending() []wire.RunEvents {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, _ := os.ReadDir(s.dir)
	var out []wire.RunEvents
	for _, ent := range entries {
		name := ent.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		runID := strings.TrimSuffix(name, ".jsonl")
		evs := s.read(runID)
		if len(evs) == 0 {
			continue
		}
		sort.Slice(evs, func(i, j int) bool { return evs[i].Seq < evs[j].Seq })
		// Send in chunks so one huge backlog does not become one huge message.
		for len(evs) > 0 {
			n := min(len(evs), 200)
			out = append(out, wire.RunEvents{RunID: runID, Events: evs[:n]})
			evs = evs[n:]
		}
	}
	return out
}

func (s *FileSpool) Done(runID string, upToSeq int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	evs := s.read(runID)
	kept := evs[:0]
	for _, e := range evs {
		if e.Seq > upToSeq {
			kept = append(kept, e)
		}
	}
	p := s.path(runID)
	if len(kept) == 0 {
		_ = os.Remove(p)
		return
	}
	tmp := p + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return
	}
	w := bufio.NewWriter(f)
	for _, e := range kept {
		line, _ := json.Marshal(e)
		_, _ = w.Write(append(line, '\n'))
	}
	_ = w.Flush()
	_ = f.Sync()
	_ = f.Close()
	_ = os.Rename(tmp, p)
}
