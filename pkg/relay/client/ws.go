package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/coder/websocket"

	v1 "github.com/wyolet/relay/pkg/relay/v1"
)

// RelayWS targets a relay server's canonical WebSocket endpoint
// (/v1/ws). One long-lived connection carries every request, so the TLS
// + auth handshake is paid once instead of per turn — the win for agent
// loops doing many sequential generations against the same relay.
//
// The connection is stateless: each turn still sends the full request
// (relay holds no conversation state). This v1 client is sequential —
// one request in flight at a time per Client; concurrent multiplexing
// from a single Client is a future enhancement. Call Close when done.
func RelayWS(baseURL, relayKey string, opts ...Option) *Client {
	c := New(v1.IdentityTranslator{}, baseURL, "/v1/ws", relayKey, opts...)
	c.transport = &wsTransport{}
	return c
}

// frame protocol — mirrors app/transport/ws on the wire. Defined here too
// because pkg/ may not import app/ (canonical purity rule 10).
type wsRequestFrame struct {
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"payload"`
}

type wsResponseFrame struct {
	ID     string `json:"id"`
	Event  string `json:"event"` // "start" | "chunk" | "end"
	Status int    `json:"status,omitempty"`
	Data   string `json:"data,omitempty"`
}

// wsTransport multiplexes requests over one WebSocket. The mutex makes the
// client sequential: a roundTrip holds it from frame-send until the caller
// Closes the response body, keeping the single connection owned by exactly
// one in-flight request. The hand-off (Lock in roundTrip, Unlock in
// wsBody.Close) is deliberate.
type wsTransport struct {
	mu     sync.Mutex
	conn   *websocket.Conn
	nextID int
}

func (t *wsTransport) roundTrip(ctx context.Context, c *Client, _ string, body []byte) (*rtResponse, error) {
	t.mu.Lock()
	if err := t.ensureConn(ctx, c); err != nil {
		t.mu.Unlock()
		return nil, err
	}

	t.nextID++
	id := strconv.Itoa(t.nextID)
	frame, err := json.Marshal(wsRequestFrame{ID: id, Payload: json.RawMessage(body)})
	if err != nil {
		t.mu.Unlock()
		return nil, fmt.Errorf("relay client: marshal ws frame: %w", err)
	}
	if err := t.conn.Write(ctx, websocket.MessageText, frame); err != nil {
		t.resetLocked()
		t.mu.Unlock()
		return nil, fmt.Errorf("relay client: ws write: %w", err)
	}

	start, err := t.readFrameFor(ctx, id)
	if err != nil {
		t.resetLocked()
		t.mu.Unlock()
		return nil, fmt.Errorf("relay client: ws read start: %w", err)
	}
	if start.Event != "start" {
		t.resetLocked()
		t.mu.Unlock()
		return nil, fmt.Errorf("relay client: expected start frame, got %q", start.Event)
	}

	return &rtResponse{status: start.Status, body: &wsBody{t: t, ctx: ctx, id: id}}, nil
}

func (t *wsTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn == nil {
		return nil
	}
	err := t.conn.Close(websocket.StatusNormalClosure, "")
	t.conn = nil
	return err
}

// ensureConn dials the WebSocket on first use. Caller holds t.mu. The dial
// ctx covers the handshake only; the connection then outlives any single
// request and is reused until an error or Close.
func (t *wsTransport) ensureConn(ctx context.Context, c *Client) error {
	if t.conn != nil {
		return nil
	}
	hdr := http.Header{}
	for k, v := range c.headers {
		hdr.Set(k, v)
	}
	c.applyAuth(hdr)

	conn, _, err := websocket.Dial(ctx, wsURL(c.baseURL)+c.path, &websocket.DialOptions{
		HTTPHeader: hdr,
		HTTPClient: c.http,
	})
	if err != nil {
		return fmt.Errorf("relay client: ws dial: %w", err)
	}
	t.conn = conn
	return nil
}

// readFrameFor reads frames until one for id arrives. Since the client is
// sequential, only id's frames are on the wire; the id guard is a
// belt-and-braces against a desynced connection.
func (t *wsTransport) readFrameFor(ctx context.Context, id string) (wsResponseFrame, error) {
	for {
		typ, data, err := t.conn.Read(ctx)
		if err != nil {
			return wsResponseFrame{}, err
		}
		if typ != websocket.MessageText {
			continue
		}
		var f wsResponseFrame
		if err := json.Unmarshal(data, &f); err != nil || f.ID != id {
			continue
		}
		return f, nil
	}
}

func (t *wsTransport) resetLocked() {
	if t.conn != nil {
		_ = t.conn.Close(websocket.StatusInternalError, "")
		t.conn = nil
	}
}

// wsBody presents the chunk-frame sequence for one request as an
// io.ReadCloser, so Generate / GenerateStream consume it exactly like an
// HTTP body. It owns t.mu until Close, which drains to the end frame so
// the connection is clean for the next request, then releases the mutex.
type wsBody struct {
	t      *wsTransport
	ctx    context.Context
	id     string
	buf    []byte
	done   bool
	closed bool
}

func (b *wsBody) Read(p []byte) (int, error) {
	for len(b.buf) == 0 {
		if b.done {
			return 0, io.EOF
		}
		f, err := b.t.readFrameFor(b.ctx, b.id)
		if err != nil {
			b.t.resetLocked()
			b.done = true
			return 0, err
		}
		switch f.Event {
		case "chunk":
			b.buf = []byte(f.Data)
		case "end":
			b.done = true
			return 0, io.EOF
		}
	}
	n := copy(p, b.buf)
	b.buf = b.buf[n:]
	return n, nil
}

func (b *wsBody) Close() error {
	if b.closed {
		return nil
	}
	b.closed = true
	// Drain to the end frame so the next request starts on a clean
	// connection. A read error means the connection is dead; reset it.
	for !b.done {
		f, err := b.t.readFrameFor(b.ctx, b.id)
		if err != nil {
			b.t.resetLocked()
			break
		}
		if f.Event == "end" {
			break
		}
	}
	b.t.mu.Unlock()
	return nil
}

func wsURL(base string) string {
	switch {
	case strings.HasPrefix(base, "https://"):
		return "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		return "ws://" + strings.TrimPrefix(base, "http://")
	default:
		return base
	}
}
