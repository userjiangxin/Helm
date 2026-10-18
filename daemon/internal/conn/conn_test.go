package conn

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/helm/helm/daemon/internal/wire"
)

type recorder struct {
	mu    sync.Mutex
	wakes []string
}

func (r *recorder) OnWake(_ context.Context, reason string) {
	r.mu.Lock()
	r.wakes = append(r.wakes, reason)
	r.mu.Unlock()
}
func (r *recorder) Heartbeat() wire.Heartbeat { return wire.Heartbeat{RunningRuns: []string{"run-1"}} }
func (r *recorder) has(reason string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, w := range r.wakes {
		if w == reason {
			return true
		}
	}
	return false
}

// Fake control plane: checks auth, sends one wake, records what the daemon sends.
func TestWakeHeartbeatAndEvents(t *testing.T) {
	ackEvents := true
	got := make(chan wire.Envelope, 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		p, _ := json.Marshal(wire.Wake{Reason: "run_queued"})
		env, _ := json.Marshal(wire.Envelope{V: 1, Type: wire.MsgWake, ID: "w1", TS: time.Now(), Payload: p})
		_ = ws.Write(r.Context(), websocket.MessageText, env)
		for {
			_, data, err := ws.Read(r.Context())
			if err != nil {
				return
			}
			var e wire.Envelope
			_ = json.Unmarshal(data, &e)
			if e.Type == wire.MsgRunEvents && ackEvents {
				var b wire.RunEvents
				_ = json.Unmarshal(e.Payload, &b)
				p, _ := json.Marshal(wire.EventsAck{RunID: b.RunID, LastSeq: b.Events[len(b.Events)-1].Seq})
				ack, _ := json.Marshal(wire.Envelope{V: 1, Type: wire.MsgEventsAck, ID: "a", TS: time.Now(), Payload: p})
				_ = ws.Write(r.Context(), websocket.MessageText, ack)
			}
			got <- e
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	spool := &MemSpool{}
	c := New(Options{ServerURL: srv.URL, Token: "tok", HeartbeatInterval: 50 * time.Millisecond, PollInterval: time.Hour}, spool)
	h := &recorder{}

	// Event published before connecting must be spooled and delivered after connect.
	c.Publish(ctx, wire.RunEvents{RunID: "run-1", Events: []wire.Event{{Seq: 1, TS: time.Now(), Type: wire.EvClaim, Data: wire.MustData(wire.Claim{Kind: "done", Text: "ok"})}}})
	go c.Run(ctx, h)

	var sawHeartbeat, sawEvents bool
	for !(sawHeartbeat && sawEvents) {
		select {
		case e := <-got:
			sawHeartbeat = sawHeartbeat || e.Type == wire.MsgHeartbeat
			sawEvents = sawEvents || e.Type == wire.MsgRunEvents
		case <-ctx.Done():
			t.Fatalf("timeout: heartbeat=%v events=%v", sawHeartbeat, sawEvents)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for !h.has("run_queued") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !h.has("run_queued") {
		t.Fatal("wake not delivered to handler")
	}
	deadline = time.Now().Add(2 * time.Second)
	for len(spool.Pending()) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := len(spool.Pending()); n != 0 {
		t.Fatalf("acked events must leave the spool, %d left", n)
	}
}

// Without an ack the batch stays spooled and is resent on the resend interval.
func TestUnackedEventsAreResent(t *testing.T) {
	sends := make(chan struct{}, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		for {
			_, data, err := ws.Read(r.Context())
			if err != nil {
				return
			}
			var e wire.Envelope
			_ = json.Unmarshal(data, &e)
			if e.Type == wire.MsgRunEvents {
				sends <- struct{}{}
			}
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	spool := &MemSpool{}
	c := New(Options{ServerURL: srv.URL, Token: "t", HeartbeatInterval: time.Hour, PollInterval: time.Hour, ResendInterval: 100 * time.Millisecond}, spool)
	go c.Run(ctx, &recorder{})
	time.Sleep(200 * time.Millisecond)
	c.Publish(ctx, wire.RunEvents{RunID: "r", Events: []wire.Event{{Seq: 1, TS: time.Now(), Type: wire.EvClaim, Data: wire.MustData(wire.Claim{Kind: "done"})}}})
	for i := 0; i < 3; i++ {
		select {
		case <-sends:
		case <-ctx.Done():
			t.Fatalf("expected repeated sends, got %d", i)
		}
	}
	if len(spool.Pending()) != 1 {
		t.Fatal("unacked batch must stay in the spool")
	}
}
