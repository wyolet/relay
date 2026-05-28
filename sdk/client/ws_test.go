package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/coder/websocket"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// wsReply is one mock response: a status and the chunk-frame payloads to
// emit between start and end.
type wsReply struct {
	status int
	chunks []string
}

// wsMock stands up a WebSocket server speaking the /v1/ws frame protocol.
// It records the upgrade Authorization header and the number of accepted
// connections so tests can assert auth + connection reuse.
type wsMock struct {
	srv      *httptest.Server
	mu       sync.Mutex
	conns    int
	lastAuth string
}

func newWSMock(t *testing.T, reply func(payload []byte) wsReply) *wsMock {
	m := &wsMock{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.conns++
		m.lastAuth = r.Header.Get("Authorization")
		m.mu.Unlock()

		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("mock accept: %v", err)
			return
		}
		ctx := context.Background()
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			var rf wsRequestFrame
			if err := json.Unmarshal(data, &rf); err != nil {
				continue
			}
			rep := reply(rf.Payload)
			write := func(f wsResponseFrame) {
				b, _ := json.Marshal(f)
				_ = conn.Write(ctx, websocket.MessageText, b)
			}
			write(wsResponseFrame{ID: rf.ID, Event: "start", Status: rep.status})
			for _, c := range rep.chunks {
				write(wsResponseFrame{ID: rf.ID, Event: "chunk", Data: c})
			}
			write(wsResponseFrame{ID: rf.ID, Event: "end", Status: rep.status})
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *wsMock) connCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.conns
}

func TestRelayWS_Sync(t *testing.T) {
	resp := &v1.Response{
		ID: "resp_ws", Object: "response", Status: v1.StatusCompleted, FinishReason: v1.FinishReasonStop,
		Output: []v1.Item{&v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{&v1.OutputTextPart{Text: "hi over ws"}}}},
	}
	body, _ := json.Marshal(resp)
	m := newWSMock(t, func([]byte) wsReply {
		return wsReply{status: 200, chunks: []string{string(body)}}
	})

	c := RelayWS(m.srv.URL, "rk-ws")
	defer c.Close()

	got, err := c.Generate(context.Background(), sampleReq())
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "resp_ws" || outputText(got) != "hi over ws" {
		t.Errorf("response: %+v", got)
	}
	if m.lastAuth != "Bearer rk-ws" {
		t.Errorf("upgrade auth: %q", m.lastAuth)
	}
}

func TestRelayWS_Stream(t *testing.T) {
	frames := []v1.SSEFrame{
		{Event: v1.EventGenerationCreated, Data: []byte(`{"id":"resp_1"}`)},
		{Event: v1.EventItemDelta, Data: []byte(`{"item_id":"msg_1","kind":"output_text"}`)},
		{Event: v1.EventGenerationCompleted, Data: []byte(`{"id":"resp_1","status":"completed","finish_reason":"stop"}`)},
	}
	var chunks []string
	for _, f := range frames {
		chunks = append(chunks, string(f.Bytes()))
	}
	m := newWSMock(t, func([]byte) wsReply {
		return wsReply{status: 200, chunks: chunks}
	})

	c := RelayWS(m.srv.URL, "rk")
	defer c.Close()

	got := drain(t, c)
	assertEvents(t, got, []string{v1.EventGenerationCreated, v1.EventItemDelta, v1.EventGenerationCompleted})
}

func TestRelayWS_APIError(t *testing.T) {
	m := newWSMock(t, func([]byte) wsReply {
		return wsReply{status: 400, chunks: []string{`{"error":{"code":"translate_request","message":"input is required"}}`}}
	})

	c := RelayWS(m.srv.URL, "rk")
	defer c.Close()

	_, err := c.Generate(context.Background(), sampleReq())
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 400 || apiErr.Code != "translate_request" {
		t.Errorf("apiErr: %+v", apiErr)
	}
}

func TestRelayWS_ConnectionReuse(t *testing.T) {
	resp := &v1.Response{ID: "r", Object: "response", Status: v1.StatusCompleted, FinishReason: v1.FinishReasonStop,
		Output: []v1.Item{&v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{&v1.OutputTextPart{Text: "ok"}}}}}
	body, _ := json.Marshal(resp)
	m := newWSMock(t, func([]byte) wsReply {
		return wsReply{status: 200, chunks: []string{string(body)}}
	})

	c := RelayWS(m.srv.URL, "rk")
	defer c.Close()

	for i := 0; i < 3; i++ {
		if _, err := c.Generate(context.Background(), sampleReq()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if n := m.connCount(); n != 1 {
		t.Errorf("expected 1 reused connection across 3 calls, got %d", n)
	}
}

// TestRelayWS_StreamEarlyClose closes the stream before draining all
// events; the next request must still succeed on the reused connection.
func TestRelayWS_StreamEarlyClose(t *testing.T) {
	frames := []v1.SSEFrame{
		{Event: v1.EventGenerationCreated, Data: []byte(`{"id":"r"}`)},
		{Event: v1.EventItemDelta, Data: []byte(`{"item_id":"m","kind":"output_text"}`)},
		{Event: v1.EventGenerationCompleted, Data: []byte(`{"id":"r","status":"completed","finish_reason":"stop"}`)},
	}
	var chunks []string
	for _, f := range frames {
		chunks = append(chunks, string(f.Bytes()))
	}
	syncResp, _ := json.Marshal(&v1.Response{ID: "r2", Object: "response", Status: v1.StatusCompleted, FinishReason: v1.FinishReasonStop,
		Output: []v1.Item{&v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{&v1.OutputTextPart{Text: "done"}}}}})
	m := newWSMock(t, func(payload []byte) wsReply {
		var probe struct {
			OutputMode string `json:"output_mode"`
		}
		_ = json.Unmarshal(payload, &probe)
		if probe.OutputMode == "stream" {
			return wsReply{status: 200, chunks: chunks}
		}
		return wsReply{status: 200, chunks: []string{string(syncResp)}}
	})

	c := RelayWS(m.srv.URL, "rk")
	defer c.Close()

	stream, err := c.GenerateStream(context.Background(), sampleReq())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil && err != io.EOF { // read one, then bail
		t.Fatal(err)
	}
	_ = stream.Close() // drains remaining frames to keep the conn clean

	// Reused connection must serve a fresh request cleanly.
	if _, err := c.Generate(context.Background(), sampleReq()); err != nil {
		t.Fatalf("request after early close failed: %v", err)
	}
	if n := m.connCount(); n != 1 {
		t.Errorf("expected connection reuse, got %d connections", n)
	}
}
