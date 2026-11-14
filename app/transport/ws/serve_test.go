package ws

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/go-cmp/cmp"
)

// serveTest upgrades on an httptest server, runs Serve with the handler and opts, and dials it. The base request passed to Serve is the real upgrade request.
func serveTest(t *testing.T, handler http.HandlerFunc, opts Options) *websocket.Conn {
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
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	t.Cleanup(func() {
		if conn != nil {
			conn.Close(websocket.StatusNormalClosure, "")
		}
		cancel()
		srv.Close()
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn
}

func send(t *testing.T, conn *websocket.Conn, typ websocket.MessageType, msg string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Write(ctx, typ, []byte(msg)); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

func sendFrame(t *testing.T, conn *websocket.Conn, id, payload string) {
	t.Helper()
	send(t, conn, websocket.MessageText, `{"id":"`+id+`","payload":`+payload+`}`)
}

// readFrames reads frames until n end frames have arrived.
func readFrames(t *testing.T, conn *websocket.Conn, n int) []serverFrame {
	t.Helper()
	var got []serverFrame
	for n > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, data, err := conn.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("read frame after %+v: %v", got, err)
		}
		var f serverFrame
		if err := json.Unmarshal(data, &f); err != nil {
			t.Fatalf("frame %q: %v", data, err)
		}
		got = append(got, f)
		if f.Event == eventEnd {
			n--
		}
	}
	return got
}

func start(id string, status int, contentType string) serverFrame {
	return serverFrame{ID: id, Event: eventStart, Status: status, Headers: map[string]string{"content-type": contentType}}
}

func chunk(id, data string) serverFrame { return serverFrame{ID: id, Event: eventChunk, Data: data} }

func end(id string, status int) serverFrame {
	return serverFrame{ID: id, Event: eventEnd, Status: status}
}

// One request's response is exactly one start, a chunk per non-empty Write, and one end carrying the status.
func TestServe_ResponseFrames(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    []serverFrame
	}{
		{
			name: "buffered body with an implicit 200",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true}`))
			},
			want: []serverFrame{start("a", 200, "application/json"), chunk("a", `{"ok":true}`), end("a", 200)},
		},
		{
			name: "one chunk per write, empty writes and flushes add none",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("data: a\n\n"))
				w.(http.Flusher).Flush()
				if n, err := w.Write(nil); n != 0 || err != nil {
					t.Errorf("empty Write = %d, %v", n, err)
				}
				if n, err := w.Write([]byte("data: b\n\n")); n != 9 || err != nil {
					t.Errorf("Write = %d, %v; want 9, nil", n, err)
				}
			},
			want: []serverFrame{start("a", 200, "text/event-stream"), chunk("a", "data: a\n\n"), chunk("a", "data: b\n\n"), end("a", 200)},
		},
		{
			name: "error status is on the start and the end; a second WriteHeader is ignored",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"error":"bad"}`))
			},
			want: []serverFrame{start("a", 400, "application/json"), chunk("a", `{"error":"bad"}`), end("a", 400)},
		},
		{
			name:    "a handler that writes nothing still starts and ends",
			handler: func(http.ResponseWriter, *http.Request) {},
			want:    []serverFrame{start("a", 200, ""), end("a", 200)},
		},
		{
			name: "a panicking handler still ends its request",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("partial"))
				panic("handler bug")
			},
			want: []serverFrame{start("a", 200, ""), chunk("a", "partial"), end("a", 200)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := serveTest(t, tt.handler, Options{})
			sendFrame(t, conn, "a", `{"model":"m"}`)
			if diff := cmp.Diff(tt.want, readFrames(t, conn, 1)); diff != "" {
				t.Errorf("frames (-want +got):\n%s", diff)
			}
		})
	}
}

// Each frame reaches the handler as a POST clone of the upgrade request whose body is the payload verbatim, under the per-request context.
func TestServe_SyntheticRequest(t *testing.T) {
	type ctxKey struct{}
	type seen struct {
		Method, Path, Body string
		ContentLength      int64
		CtxValue           any
	}
	got := make(chan seen, 1)
	h := func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- seen{r.Method, r.URL.Path, string(b), r.ContentLength, r.Context().Value(ctxKey{})}
	}
	conn := serveTest(t, h, Options{PerRequest: func(parent context.Context) context.Context {
		return context.WithValue(parent, ctxKey{}, "per-request")
	}})
	sendFrame(t, conn, "a", `{"model": "m",  "n":[1,2]}`)
	readFrames(t, conn, 1)

	want := seen{Method: http.MethodPost, Path: "/", Body: `{"model": "m",  "n":[1,2]}`, ContentLength: 26, CtxValue: "per-request"}
	if diff := cmp.Diff(want, <-got); diff != "" {
		t.Errorf("handler request (-want +got):\n%s", diff)
	}
}

func TestServe_Multiplexed(t *testing.T) {
	h := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.Copy(w, r.Body)
	}
	conn := serveTest(t, h, Options{})
	sendFrame(t, conn, "one", `{"n":1}`)
	sendFrame(t, conn, "two", `{"n":2}`)

	// Frames of different ids interleave; each id's own frames stay in order.
	byID := map[string][]serverFrame{}
	for _, f := range readFrames(t, conn, 2) {
		byID[f.ID] = append(byID[f.ID], f)
	}
	want := map[string][]serverFrame{
		"one": {start("one", 200, "application/json"), chunk("one", `{"n":1}`), end("one", 200)},
		"two": {start("two", 200, "application/json"), chunk("two", `{"n":2}`), end("two", 200)},
	}
	if diff := cmp.Diff(want, byID); diff != "" {
		t.Errorf("frames by id (-want +got):\n%s", diff)
	}
}

// A frame arriving while every slot is taken is answered 429 at once; the slot's holder is unaffected and the slot is reusable afterwards.
func TestServe_Saturation(t *testing.T) {
	release := make(chan struct{})
	h := func(w http.ResponseWriter, r *http.Request) {
		<-release
		_, _ = w.Write([]byte("late"))
	}
	conn := serveTest(t, h, Options{MaxInFlight: 1})

	sendFrame(t, conn, "blocker", `{}`)
	sendFrame(t, conn, "rejected", `{}`)
	wantRejected := []serverFrame{
		start("rejected", 429, "application/json"),
		chunk("rejected", `{"error":{"type":"rate_limit_error","code":"connection_saturated","message":"too many in-flight requests on this connection"}}`),
		end("rejected", 429),
	}
	if diff := cmp.Diff(wantRejected, readFrames(t, conn, 1)); diff != "" {
		t.Errorf("saturated frames (-want +got):\n%s", diff)
	}

	close(release)
	if diff := cmp.Diff([]serverFrame{start("blocker", 200, ""), chunk("blocker", "late"), end("blocker", 200)}, readFrames(t, conn, 1)); diff != "" {
		t.Errorf("blocker frames (-want +got):\n%s", diff)
	}
	sendFrame(t, conn, "next", `{}`)
	if diff := cmp.Diff([]serverFrame{start("next", 200, ""), chunk("next", "late"), end("next", 200)}, readFrames(t, conn, 1)); diff != "" {
		t.Errorf("frames after the slot freed (-want +got):\n%s", diff)
	}
}

// A message that cannot be tied to a request id produces no frames and leaves the connection usable.
func TestServe_UncorrelatableMessagesAreDropped(t *testing.T) {
	conn := serveTest(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }, Options{})

	send(t, conn, websocket.MessageText, `{"payload":{}}`)
	send(t, conn, websocket.MessageText, `{"id":"","payload":{}}`)
	send(t, conn, websocket.MessageText, `not json`)
	send(t, conn, websocket.MessageBinary, `{"id":"binary","payload":{}}`)
	sendFrame(t, conn, "ok", `{}`)

	if diff := cmp.Diff([]serverFrame{start("ok", 200, ""), chunk("ok", "ok"), end("ok", 200)}, readFrames(t, conn, 1)); diff != "" {
		t.Errorf("frames (-want +got):\n%s", diff)
	}
}

// A message over the read limit ends the connection instead of being buffered.
func TestServe_ReadLimit(t *testing.T) {
	conn := serveTest(t, func(http.ResponseWriter, *http.Request) {}, Options{ReadLimit: 64})
	sendFrame(t, conn, "big", `"`+strings.Repeat("x", 200)+`"`)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
		t.Fatalf("read = %q, %v; want the connection closed as message-too-big", data, err)
	}
}
