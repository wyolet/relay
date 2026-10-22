package inference

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The upstream gets exactly the client's bytes, with the client's Content-Length, whether the forward waited for the model or not, and post-flight sees the body's length and model.
func TestProxyShape_ForwardsTheBody(t *testing.T) {
	up, got := recordingUpstream(t)
	d, pr, _ := proxyStreamDeps(t, up.URL)
	seen := watchPostFlight(&d)

	small := proxyTestBody("test-model", 2<<10, false)
	cases := []struct {
		name   string
		req    proxyShapeCall
		body   []byte
		length int64
	}{
		{name: "small body", req: proxyShapeCall{principal: pr}, body: small, length: int64(len(small))},
		{name: "small body, chunked", req: proxyShapeCall{principal: pr}, body: small, length: -1},
		{name: "small body, model late", req: proxyShapeCall{principal: pr}, body: proxyTestBody("test-model", 2<<10, true)},
		{name: "large body, model first", req: proxyShapeCall{principal: pr}, body: claudeCodeSizedBody(t, 2<<20, false)},
		{name: "large body, model late", req: proxyShapeCall{principal: pr}, body: claudeCodeSizedBody(t, 2<<20, true)},
		{name: "large body, model late, chunked", req: proxyShapeCall{principal: pr}, body: claudeCodeSizedBody(t, 2<<20, true), length: -1},
		{name: "pinned host", req: proxyShapeCall{principal: pr, pin: "anthropic"}, body: claudeCodeSizedBody(t, 1<<20, true)},
		{name: "anonymous pinned host", req: proxyShapeCall{pin: "anthropic"}, body: claudeCodeSizedBody(t, 1<<20, true)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			req.body = bytes.NewReader(tc.body)
			req.length = int64(len(tc.body))
			if tc.length != 0 {
				req.length = tc.length
			}
			if w := serveProxyShape(d, req); w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			g := <-got
			if g.readErr != nil || !bytes.Equal(g.body, tc.body) {
				t.Fatalf("upstream got %d bytes (%v), want the client's %d", len(g.body), g.readErr, len(tc.body))
			}
			if g.contentLength != req.length {
				t.Fatalf("upstream Content-Length %d, want %d", g.contentLength, req.length)
			}
			s := awaitPostFlight(t, seen)
			if s.requestBytes != len(tc.body) || s.requestedModel != "test-model" {
				t.Fatalf("post-flight saw %d bytes and model %q", s.requestBytes, s.requestedModel)
			}
		})
	}
}

// Payload capture still gets the whole request body, up to its cap, though the body streamed past relay.
func TestProxyShape_PayloadCapture(t *testing.T) {
	up, got := recordingUpstream(t)
	d, pr, _ := proxyStreamDeps(t, up.URL)
	seen := watchPostFlight(&d)
	logged := withPayloadLogging(pr)

	cases := []struct {
		name      string
		principal *Principal
		body      []byte
		stored    int // -1 = the whole body
		truncated bool
	}{
		{"small body", logged, proxyTestBody("test-model", 2<<10, false), -1, false},
		{"model first", logged, proxyTestBody("test-model", 3<<20, false), -1, false},
		{"model late", logged, proxyTestBody("test-model", 3<<20, true), -1, false},
		{"past the capture cap", logged, proxyTestBody("test-model", proxyMaxBodyBuffer, false), proxyMaxBodyBuffer, true},
		{"logging off", pr, proxyTestBody("test-model", 3<<20, false), 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := serveProxyShape(d, proxyShapeCall{principal: tc.principal, body: bytes.NewReader(tc.body), length: int64(len(tc.body))})
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			if g := <-got; !bytes.Equal(g.body, tc.body) {
				t.Fatalf("upstream got %d bytes, want %d", len(g.body), len(tc.body))
			}
			s := awaitPostFlight(t, seen)
			want := tc.body
			if tc.stored >= 0 {
				want = tc.body[:tc.stored]
			}
			if !bytes.Equal(s.requestBody, want) || s.truncated != tc.truncated {
				t.Fatalf("captured %d bytes (truncated %v), want %d (truncated %v)", len(s.requestBody), s.truncated, len(want), tc.truncated)
			}
		})
	}
}

// A second model is refused before the forward when it is in the held part, and aborts it when it arrives later: the upstream never holds a complete body, the reservation settles as cancelled, and the request is recorded as the refusal.
func TestProxyShape_DuplicateModel(t *testing.T) {
	up, got := recordingUpstream(t)
	d, pr, store := proxyStreamDeps(t, up.URL)
	seen := watchPostFlight(&d)

	early := []byte(`{"model":"test-model","model":"other","messages":[]}`)
	assertDuplicateModel(t, serveProxyShape(d, proxyShapeCall{principal: pr, body: bytes.NewReader(early), length: int64(len(early))}))
	select {
	case g := <-got:
		t.Fatalf("upstream called with %d bytes", len(g.body))
	default:
	}

	first := claudeCodeSizedBody(t, 2<<20, false)
	late := append(first[:len(first)-1:len(first)-1], `,"model":"other"}`...)
	assertDuplicateModel(t, serveProxyShape(d, proxyShapeCall{principal: pr, body: bytes.NewReader(late), length: int64(len(late))}))
	select {
	case g := <-got:
		if g.readErr == nil && json.Valid(g.body) {
			t.Fatalf("upstream received a complete body (%d bytes)", len(g.body))
		}
	case <-time.After(5 * time.Second):
	}
	if s := awaitPostFlight(t, seen); s.errorKind != "duplicate_model" {
		t.Fatalf("recorded as %q, want duplicate_model", s.errorKind)
	}
	commits := store.Named("limit.commit")
	if len(commits) != 1 || len(commits[0].Args) < 7 || commits[0].Args[6] != int64(1) {
		t.Fatalf("reservation settlement: %+v, want one cancelled commit", commits)
	}
}

// Proxy mode forwards the body as sent, so a model key spelled in another case or with escapes, which a case-sensitive upstream would not read, is refused, whether the forward waited for the model or not.
func TestProxyShape_ModelKeySpelling(t *testing.T) {
	up, got := recordingUpstream(t)
	d, pr, _ := proxyStreamDeps(t, up.URL)
	for _, body := range []string{`{"Model":"test-model","messages":[]}`, `{"` + uEscape("006d") + `odel":"test-model","messages":[]}`} {
		for _, req := range []proxyShapeCall{{principal: pr}, {pin: "anthropic"}} {
			req.body, req.length = strings.NewReader(body), int64(len(body))
			w := serveProxyShape(d, req)
			if w.Code != http.StatusBadRequest || parseDispatchErr(t, w.Body.Bytes()).Error.Code != "invalid_model_key" {
				t.Fatalf("%s (pin %q): status %d %s, want 400 invalid_model_key", body, req.pin, w.Code, w.Body)
			}
		}
	}
	select {
	case g := <-got:
		if g.readErr == nil {
			t.Fatalf("upstream read a complete body: %s", g.body)
		}
	default:
	}
}

// A client that disconnects mid-body gets read_body, as in normal mode, and the upstream request is aborted.
func TestProxyShape_ClientAbortMidBody(t *testing.T) {
	up, got := recordingUpstream(t)
	d, pr, _ := proxyStreamDeps(t, up.URL)
	seen := watchPostFlight(&d)
	body := claudeCodeSizedBody(t, 2<<20, false)

	pr2, pw := io.Pipe()
	go func() {
		_, _ = pw.Write(body[:len(body)/2])
		_ = pw.CloseWithError(io.ErrUnexpectedEOF)
	}()
	w := serveProxyShape(d, proxyShapeCall{principal: pr, body: pr2, length: int64(len(body))})
	if w.Code != http.StatusBadRequest || parseDispatchErr(t, w.Body.Bytes()).Error.Code != "read_body" {
		t.Fatalf("status %d: %s, want 400 read_body", w.Code, w.Body)
	}
	select {
	case g := <-got:
		if g.readErr == nil {
			t.Fatalf("upstream read a complete body (%d bytes)", len(g.body))
		}
	case <-time.After(5 * time.Second):
	}
	if s := awaitPostFlight(t, seen); s.errorKind != "read_body" {
		t.Fatalf("recorded as %q, want read_body", s.errorKind)
	}
}

// pacedUpstream signals once it has received threshold body bytes, then reads the rest.
func pacedUpstream(t *testing.T, threshold int) (*httptest.Server, <-chan struct{}, <-chan []byte) {
	t.Helper()
	reached := make(chan struct{})
	bodies := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got bytes.Buffer
		buf := make([]byte, 32<<10)
		signaled := false
		for {
			n, err := r.Body.Read(buf)
			got.Write(buf[:n])
			if !signaled && got.Len() >= threshold {
				signaled = true
				close(reached)
			}
			if err != nil {
				break
			}
		}
		bodies <- got.Bytes()
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv, reached, bodies
}

// sendPaced writes body up to cut, waits until the upstream has received enough to prove it is streaming, then writes the rest.
func sendPaced(body []byte, cut int, reached <-chan struct{}) io.Reader {
	r, w := io.Pipe()
	go func() {
		_, _ = w.Write(body[:cut])
		<-reached
		_, _ = w.Write(body[cut:])
		_ = w.Close()
	}()
	return r
}

// With the model early, the upstream receives the body while the client is still sending it; payload capture still gets all of it.
func TestProxyShape_StreamsWhileClientSending(t *testing.T) {
	body := proxyTestBody("test-model", 2<<20, false)
	up, reached, bodies := pacedUpstream(t, 1<<20)
	d, pr, _ := proxyStreamDeps(t, up.URL)
	seen := watchPostFlight(&d)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- serveProxyShape(d, proxyShapeCall{principal: withPayloadLogging(pr), body: sendPaced(body, len(body)-256<<10, reached), length: int64(len(body))})
	}()
	select {
	case w := <-done:
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("watchdog: upstream never received the body while the client was still sending")
	}
	if got := <-bodies; !bytes.Equal(got, body) {
		t.Fatalf("upstream got %d bytes, want %d", len(got), len(body))
	}
	if s := awaitPostFlight(t, seen); !bytes.Equal(s.requestBody, body) || s.truncated {
		t.Fatalf("captured %d bytes (truncated %v), want all %d", len(s.requestBody), s.truncated, len(body))
	}
}

// An anonymous pinned request does not wait for its model: the body streams from its first bytes, and the model is still recorded.
func TestProxyShape_AnonymousPinHoldsNothing(t *testing.T) {
	body := proxyTestBody("test-model", 2<<20, true)
	up, reached, bodies := pacedUpstream(t, 64<<10)
	d, _, _ := proxyStreamDeps(t, up.URL)
	seen := watchPostFlight(&d)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- serveProxyShape(d, proxyShapeCall{pin: "anthropic", body: sendPaced(body, 128<<10, reached), length: int64(len(body))})
	}()
	select {
	case w := <-done:
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("watchdog: the forward waited for a model it does not need")
	}
	if got := <-bodies; !bytes.Equal(got, body) {
		t.Fatalf("upstream got %d bytes, want %d", len(got), len(body))
	}
	if s := awaitPostFlight(t, seen); s.requestedModel != "test-model" {
		t.Fatalf("recorded model %q", s.requestedModel)
	}
}
