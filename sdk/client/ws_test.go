package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
	"github.com/google/go-cmp/cmp"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// wsMock speaks the /v1/ws frame protocol: for each request frame it writes the frames reply returns, stamped with the request's id. It records every upgrade and request frame.
type wsMock struct {
	srv      *httptest.Server
	mu       sync.Mutex
	upgrades []sentRequest
	frames   []wsRequestFrame
}

func newWSMock(t *testing.T, reply func(payload []byte) []wsResponseFrame) *wsMock {
	t.Helper()
	m := &wsMock{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.upgrades = append(m.upgrades, sentRequest{Path: r.URL.Path, Header: r.Header.Clone()})
		m.mu.Unlock()
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("mock accept: %v", err)
			return
		}
		ctx := context.Background()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var rf wsRequestFrame
			if err := json.Unmarshal(data, &rf); err != nil {
				t.Errorf("request frame is not JSON: %s", data)
				return
			}
			m.mu.Lock()
			m.frames = append(m.frames, rf)
			m.mu.Unlock()
			// A binary frame and a frame for another request must both be ignored by the client.
			_ = conn.Write(ctx, websocket.MessageBinary, []byte{0})
			_ = conn.Write(ctx, websocket.MessageText, []byte(`{"id":"someone-else","event":"start","status":500}`))
			for _, f := range reply(rf.Payload) {
				f.ID = rf.ID
				b, _ := json.Marshal(f)
				_ = conn.Write(ctx, websocket.MessageText, b)
			}
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

// ok answers 200 with body split into the given chunks.
func ok(chunks ...string) []wsResponseFrame {
	frames := []wsResponseFrame{{Event: "start", Status: 200}}
	for _, c := range chunks {
		frames = append(frames, wsResponseFrame{Event: "chunk", Data: c})
	}
	return append(frames, wsResponseFrame{Event: "end", Status: 200})
}

func (m *wsMock) sent() (ids, payloads []string, upgrades []sentRequest) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range m.frames {
		ids, payloads = append(ids, f.ID), append(payloads, string(f.Payload))
	}
	return ids, payloads, m.upgrades
}

func TestRelayWS_Generate(t *testing.T) {
	// The body arrives in two chunk frames; the client must join them.
	m := newWSMock(t, func([]byte) []wsResponseFrame { return ok(canonicalReply[:20], canonicalReply[20:]) })
	t.Setenv(EnvHeaders, "")
	c := RelayWS(m.srv.URL, "rk-ws", WithHeader("X-Team", "red"))
	defer c.Close()

	want, err := v1.ParseResponse([]byte(canonicalReply))
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		got, err := c.Generate(context.Background(), sampleReq())
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(want, got.Response); diff != "" {
			t.Errorf("response (-want +got):\n%s", diff)
		}
	}

	ids, payloads, upgrades := m.sent()
	if diff := cmp.Diff([]string{"1", "2", "3"}, ids); diff != "" {
		t.Errorf("frame ids (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{sampleWire("sync"), sampleWire("sync"), sampleWire("sync")}, payloads); diff != "" {
		t.Errorf("frame payloads (-want +got):\n%s", diff)
	}
	if len(upgrades) != 1 || upgrades[0].Path != "/v1/ws" || upgrades[0].Header.Get("Authorization") != "Bearer rk-ws" || upgrades[0].Header.Get("X-Team") != "red" {
		t.Errorf("upgrades = %+v, want one to /v1/ws carrying the key and headers", upgrades)
	}
}

func TestRelayWS_Stream(t *testing.T) {
	m := newWSMock(t, func(payload []byte) []wsResponseFrame {
		if string(payload) == sampleWire("stream") {
			return ok(strings.SplitAfter(canonicalStream, "\n\n")...)
		}
		return ok(canonicalReply)
	})
	c := RelayWS(m.srv.URL, "rk")
	defer c.Close()

	want := []string{v1.EventGenerationCreated, v1.EventItemDelta, v1.EventGenerationCompleted}
	if diff := cmp.Diff(want, eventTypes(drain(t, c))); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}

	// A stream closed before its end frame must leave the connection clean for the next request.
	stream, err := c.GenerateStream(context.Background(), sampleReq())
	if err != nil {
		t.Fatal(err)
	}
	if ev, err := stream.Recv(); err != nil || ev.Type != v1.EventGenerationCreated {
		t.Fatalf("first event = %v, %v", ev, err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	resp, err := c.Generate(context.Background(), sampleReq())
	if err != nil || resp.ID != "resp_1" {
		t.Fatalf("request after an early close: %v, %v", resp, err)
	}
	if ids, _, upgrades := m.sent(); len(upgrades) != 1 || len(ids) != 3 {
		t.Errorf("%d connections for %d requests, want one reused connection for 3", len(upgrades), len(ids))
	}
}

func TestRelayWS_APIError(t *testing.T) {
	m := newWSMock(t, func([]byte) []wsResponseFrame {
		return []wsResponseFrame{{Event: "start", Status: 400}, {Event: "chunk", Data: `{"error":{"code":"translate_request","message":"input is required"}}`}, {Event: "end", Status: 400}}
	})
	c := RelayWS(m.srv.URL, "rk")
	defer c.Close()

	_, err := c.Generate(context.Background(), sampleReq())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want an *APIError", err)
	}
	want := &APIError{StatusCode: 400, Host: strings.TrimPrefix(m.srv.URL, "http://"), Code: "translate_request", Message: "input is required",
		Raw: []byte(`{"error":{"code":"translate_request","message":"input is required"}}`)}
	if diff := cmp.Diff(want, apiErr); diff != "" {
		t.Errorf("error (-want +got):\n%s", diff)
	}
}

// A reply that does not open with a start frame fails the call and drops the connection, so the next call dials again.
func TestRelayWS_ProtocolErrorResetsTheConnection(t *testing.T) {
	bad := true
	m := newWSMock(t, func([]byte) []wsResponseFrame {
		if bad {
			bad = false
			return []wsResponseFrame{{Event: "chunk", Data: "x"}}
		}
		return ok(canonicalReply)
	})
	c := RelayWS(m.srv.URL, "rk")
	defer c.Close()

	if _, err := c.Generate(context.Background(), sampleReq()); errText(err) != `relay client: expected start frame, got "chunk"` {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.Generate(context.Background(), sampleReq()); err != nil {
		t.Fatalf("call after a protocol error: %v", err)
	}
	if _, _, upgrades := m.sent(); len(upgrades) != 2 {
		t.Errorf("%d connections, want a fresh one after the protocol error", len(upgrades))
	}
}

func TestRelayWS_DialFailure(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	c := RelayWS(srv.URL, "rk")
	if _, err := c.Generate(context.Background(), sampleReq()); err == nil || !strings.HasPrefix(err.Error(), "relay client: ws dial: ") {
		t.Errorf("err = %v, want a ws dial error", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close without a connection = %v", err)
	}
}

func TestWSAndHTTPURL(t *testing.T) {
	for _, tc := range []struct{ base, ws, http string }{
		{"https://relay.example/x", "wss://relay.example/x", "https://relay.example/x"},
		{"http://relay.example", "ws://relay.example", "http://relay.example"},
		{"wss://relay.example", "wss://relay.example", "https://relay.example"},
		{"ws://relay.example", "ws://relay.example", "http://relay.example"},
		{"relay.example", "relay.example", "relay.example"},
		{"", "", ""},
	} {
		if got := wsURL(tc.base); got != tc.ws {
			t.Errorf("wsURL(%q) = %q, want %q", tc.base, got, tc.ws)
		}
		if got := httpURL(tc.base); got != tc.http {
			t.Errorf("httpURL(%q) = %q, want %q", tc.base, got, tc.http)
		}
	}
}
