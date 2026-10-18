// Package conn is helmd's only link to the control plane.
//
// Pattern (borrowed from Multica's daemon): the WebSocket is a hint channel —
// the server pushes `wake`, the daemon then pulls work over HTTP. A periodic poll
// covers missed wakes and outages. Events go up over the WebSocket and are buffered
// in a Spool while disconnected, then resent by seq.
package conn

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/helm/helm/daemon/internal/wire"
)

type Options struct {
	ServerURL         string // e.g. https://helm.tailnet-name.ts.net
	Token             string // machine token
	HeartbeatInterval time.Duration
	PollInterval      time.Duration
	Log               *slog.Logger
}

// Handler receives control-plane signals. Implemented by the supervisor.
type Handler interface {
	// OnWake is called on a `wake` message and on every poll tick; it should claim
	// runs and fetch directives over HTTP. Must be safe to call concurrently.
	OnWake(ctx context.Context, reason string)
	// Heartbeat returns the current heartbeat payload.
	Heartbeat() wire.Heartbeat
}

type Client struct {
	opt   Options
	http  *http.Client
	spool Spool

	mu sync.Mutex
	ws *websocket.Conn
}

func New(opt Options, spool Spool) *Client {
	if opt.HeartbeatInterval == 0 {
		opt.HeartbeatInterval = 15 * time.Second
	}
	if opt.PollInterval == 0 {
		opt.PollInterval = 30 * time.Second
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	opt.ServerURL = strings.TrimRight(opt.ServerURL, "/")
	return &Client{opt: opt, http: &http.Client{Timeout: 30 * time.Second}, spool: spool}
}

// ---------- HTTP (pull) ----------

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.opt.ServerURL+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.opt.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, msg)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) PutCapabilities(ctx context.Context, caps wire.Capabilities) error {
	return c.do(ctx, http.MethodPut, "/daemon/capabilities", caps, nil)
}

// Claim asks for up to `slots` runs. The server assigns lease + epoch atomically.
func (c *Client) Claim(ctx context.Context, slots int) ([]wire.RunAssignment, error) {
	var out struct {
		Runs []wire.RunAssignment `json:"runs"`
	}
	err := c.do(ctx, http.MethodPost, "/daemon/claim", map[string]int{"slots": slots}, &out)
	return out.Runs, err
}

func (c *Client) Directives(ctx context.Context, after string) ([]wire.Directive, error) {
	var out struct {
		Directives []wire.Directive `json:"directives"`
	}
	err := c.do(ctx, http.MethodGet, "/daemon/directives?after="+after, nil, &out)
	return out.Directives, err
}

func (c *Client) Ack(ctx context.Context, a wire.DirectiveAck) error {
	return c.do(ctx, http.MethodPost, "/daemon/directives/"+a.DirectiveID+"/ack", a, nil)
}

// ---------- events (push, spooled) ----------

// Publish queues events for a run; they are sent now if connected, else on reconnect.
func (c *Client) Publish(ctx context.Context, batch wire.RunEvents) {
	c.spool.Put(batch)
	c.flush(ctx)
}

func (c *Client) flush(ctx context.Context) {
	c.mu.Lock()
	ws := c.ws
	c.mu.Unlock()
	if ws == nil {
		return
	}
	for _, b := range c.spool.Pending() {
		if err := c.send(ctx, ws, wire.MsgRunEvents, b); err != nil {
			c.opt.Log.Warn("event flush failed; will retry", "err", err)
			return
		}
		// TODO(M0-D3): delete only after the server acks the highest seq per run.
		c.spool.Done(b.RunID, b.Events[len(b.Events)-1].Seq)
	}
}

func (c *Client) send(ctx context.Context, ws *websocket.Conn, t wire.MsgType, payload any) error {
	p, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	env, _ := json.Marshal(wire.Envelope{V: wire.Version, Type: t, ID: newID(), TS: time.Now().UTC(), Payload: p})
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return ws.Write(ctx, websocket.MessageText, env)
}

// ---------- WebSocket loop ----------

// Run keeps the WebSocket alive (with jittered backoff) and runs the poll loop. Blocks until ctx ends.
func (c *Client) Run(ctx context.Context, h Handler) {
	go c.pollLoop(ctx, h)
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := c.session(ctx, h)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		c.opt.Log.Warn("websocket disconnected", "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff + time.Duration(rand.Int64N(int64(backoff/2)+1))):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (c *Client) session(ctx context.Context, h Handler) error {
	wsURL := "ws" + strings.TrimPrefix(c.opt.ServerURL, "http") + "/daemon/ws"
	ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + c.opt.Token}},
	})
	if err != nil {
		return err
	}
	defer ws.CloseNow()
	c.mu.Lock()
	c.ws = ws
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.ws = nil; c.mu.Unlock() }()

	c.opt.Log.Info("connected to control plane", "url", wsURL)
	c.flush(ctx)
	go h.OnWake(ctx, "reconnected")

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		t := time.NewTicker(c.opt.HeartbeatInterval)
		defer t.Stop()
		for {
			if err := c.send(sctx, ws, wire.MsgHeartbeat, h.Heartbeat()); err != nil {
				cancel()
				return
			}
			select {
			case <-sctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	for {
		_, data, err := ws.Read(sctx)
		if err != nil {
			return err
		}
		var env wire.Envelope
		if err := json.Unmarshal(data, &env); err != nil || env.V != wire.Version {
			c.opt.Log.Warn("ignoring malformed message")
			continue
		}
		if env.Type == wire.MsgWake {
			var w wire.Wake
			_ = json.Unmarshal(env.Payload, &w)
			go h.OnWake(ctx, w.Reason)
		}
	}
}

func (c *Client) pollLoop(ctx context.Context, h Handler) {
	t := time.NewTicker(c.opt.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.OnWake(ctx, "poll")
		}
	}
}

func newID() string {
	return fmt.Sprintf("%x%08x", time.Now().UnixNano(), rand.Uint32())
}
