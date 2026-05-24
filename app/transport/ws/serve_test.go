package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// serveTest spins up an httptest server that upgrades and runs Serve with
// the given per-frame handler + opts, then dials it. The base request
// passed to Serve is the real upgrade request, mirroring production.
func serveTest(t *testing.T, handler http.HandlerFunc, opts Options) (*websocket.Conn, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		_ = Serve(r.Context(), conn, r, handler, opts)
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		cancel()
		srv.Close()
		t.Fatalf("dial: %v", err)
	}
	cleanup := func() {
		conn.Close(websocket.StatusNormalClosure, "")
		cancel()
		srv.Close()
	}
	return conn, cleanup
}

func sendFrame(t *testing.T, conn *websocket.Conn, id, payload string) {
	t.Helper()
	b, _ := json.Marshal(clientFrame{ID: id, Payload: json.RawMessage(payload)})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

func readFrame(t *testing.T, conn *websocket.Conn) serverFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var f serverFrame
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("unmarshal frame: %v", err)
	}
	return f
}

// collectUntilEnd reads frames for one id until its end frame.
func collectUntilEnd(t *testing.T, conn *websocket.Conn) []serverFrame {
	t.Helper()
	var got []serverFrame
	for {
		f := readFrame(t, conn)
		got = append(got, f)
		if f.Event == eventEnd {
			return got
		}
	}
}

func TestServe_BufferedResponse(t *testing.T) {
	h := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
	conn, cleanup := serveTest(t, h, Options{})
	defer cleanup()

	sendFrame(t, conn, "a", `{"model":"m"}`)
	got := collectUntilEnd(t, conn)

	if len(got) != 3 {
		t.Fatalf("want 3 frames (start,chunk,end), got %d: %+v", len(got), got)
	}
	if got[0].Event != eventStart || got[0].Status != 200 {
		t.Errorf("start frame wrong: %+v", got[0])
	}
	if got[0].Headers["content-type"] != "application/json" {
		t.Errorf("content-type not propagated: %+v", got[0].Headers)
	}
	if got[1].Event != eventChunk || got[1].Data != `{"ok":true}` {
		t.Errorf("chunk frame wrong: %+v", got[1])
	}
	if got[2].Event != eventEnd {
		t.Errorf("end frame wrong: %+v", got[2])
	}
	for _, f := range got {
		if f.ID != "a" {
			t.Errorf("frame id not echoed: %+v", f)
		}
	}
}

func TestServe_StreamingChunks(t *testing.T) {
	h := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: a\n\n"))
		_, _ = w.Write([]byte("data: b\n\n"))
	}
	conn, cleanup := serveTest(t, h, Options{})
	defer cleanup()

	sendFrame(t, conn, "s", `{"model":"m","output_mode":"stream"}`)
	got := collectUntilEnd(t, conn)

	var chunks []string
	for _, f := range got {
		if f.Event == eventChunk {
			chunks = append(chunks, f.Data)
		}
	}
	if len(chunks) != 2 || chunks[0] != "data: a\n\n" || chunks[1] != "data: b\n\n" {
		t.Errorf("streaming chunks wrong: %v", chunks)
	}
}

func TestServe_ErrorStatus(t *testing.T) {
	h := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad"}`))
	}
	conn, cleanup := serveTest(t, h, Options{})
	defer cleanup()

	sendFrame(t, conn, "e", `{"model":"m"}`)
	got := collectUntilEnd(t, conn)

	if got[0].Status != http.StatusBadRequest {
		t.Errorf("want status 400 in start frame, got %d", got[0].Status)
	}
	if got[len(got)-1].Status != http.StatusBadRequest {
		t.Errorf("want status 400 in end frame, got %d", got[len(got)-1].Status)
	}
}

func TestServe_Multiplexed(t *testing.T) {
	// Handler echoes the request body so we can tell responses apart.
	h := func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(buf)
	}
	conn, cleanup := serveTest(t, h, Options{})
	defer cleanup()

	sendFrame(t, conn, "one", `{"n":1}`)
	sendFrame(t, conn, "two", `{"n":2}`)

	byID := map[string][]serverFrame{}
	for done := 0; done < 2; {
		f := readFrame(t, conn)
		byID[f.ID] = append(byID[f.ID], f)
		if f.Event == eventEnd {
			done++
		}
	}

	if len(byID["one"]) != 3 || len(byID["two"]) != 3 {
		t.Fatalf("each id should have 3 frames: %+v", byID)
	}
	if byID["one"][1].Data != `{"n":1}` {
		t.Errorf("id one body wrong: %q", byID["one"][1].Data)
	}
	if byID["two"][1].Data != `{"n":2}` {
		t.Errorf("id two body wrong: %q", byID["two"][1].Data)
	}
}

func TestServe_Saturation(t *testing.T) {
	release := make(chan struct{})
	h := func(w http.ResponseWriter, r *http.Request) {
		<-release // block the single in-flight slot
		_, _ = w.Write([]byte("late"))
	}
	conn, cleanup := serveTest(t, h, Options{MaxInFlight: 1})
	defer cleanup()
	defer close(release)

	sendFrame(t, conn, "blocker", `{"model":"m"}`) // takes the slot
	sendFrame(t, conn, "rejected", `{"model":"m"}`)

	// The rejected frame's full start→chunk→end should arrive first since
	// the blocker is parked in the handler.
	got := collectUntilEnd(t, conn)
	if got[0].ID != "rejected" {
		t.Fatalf("expected rejected response first, got id %q", got[0].ID)
	}
	if got[0].Status != http.StatusTooManyRequests {
		t.Errorf("want 429, got %d", got[0].Status)
	}
}

func TestServe_MalformedFrameDropped(t *testing.T) {
	h := func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}
	conn, cleanup := serveTest(t, h, Options{})
	defer cleanup()

	// Missing id — should be silently dropped, connection still usable.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = conn.Write(ctx, websocket.MessageText, []byte(`{"payload":{}}`))
	cancel()

	sendFrame(t, conn, "ok", `{"model":"m"}`)
	got := collectUntilEnd(t, conn)
	if got[0].ID != "ok" {
		t.Errorf("expected only the well-formed frame's response, got %q", got[0].ID)
	}
}
