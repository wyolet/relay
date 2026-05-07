package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/transport"
)

func collectMessages(t *testing.T, c *Client, body []byte, secret string) []*transport.Message {
	t.Helper()
	out := make(chan *transport.Message, 64)
	ctx := context.Background()
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.ChatCompletions(ctx, body, secret, out)
	}()
	var msgs []*transport.Message
	for m := range out {
		msgs = append(msgs, m)
	}
	<-errCh
	return msgs
}

func TestChatCompletions_AuthHeader(t *testing.T) {
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(srv.URL)
	collectMessages(t, c, []byte(`{}`), "sk-test-key")

	want := "Bearer sk-test-key"
	if captured != want {
		t.Errorf("Authorization header = %q; want %q", captured, want)
	}
}

func TestChatCompletions_BodyForwarded(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[]}`)
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(srv.URL)
	collectMessages(t, c, body, "key")

	if string(got) != string(body) {
		t.Errorf("body forwarded = %q; want %q", got, body)
	}
}

func TestChatCompletions_StatusAndContentTypePropagated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(srv.URL)
	msgs := collectMessages(t, c, []byte(`{}`), "key")

	if len(msgs) == 0 {
		t.Fatal("no messages emitted")
	}
	first := msgs[0]
	if first.Headers["X-Relay-Status"] != "200" {
		t.Errorf("X-Relay-Status = %q; want 200", first.Headers["X-Relay-Status"])
	}
	if first.Headers["Content-Type"] != "application/json" {
		t.Errorf("Content-Type = %q; want application/json", first.Headers["Content-Type"])
	}
}

func TestChatCompletions_StreamingChunks(t *testing.T) {
	chunks := []string{"data: chunk1\n\n", "data: chunk2\n\n", "data: chunk3\n\n"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, ch := range chunks {
			w.Write([]byte(ch))
			flusher.Flush()
		}
	}))
	defer srv.Close()

	c := New(srv.URL)
	msgs := collectMessages(t, c, []byte(`{}`), "key")

	// Collect body chunks (skip first header msg, skip final marker)
	var bodyMsgs []string
	for _, m := range msgs {
		if len(m.Body) > 0 {
			bodyMsgs = append(bodyMsgs, string(m.Body))
		}
	}
	combined := strings.Join(bodyMsgs, "")
	wantCombined := strings.Join(chunks, "")
	if combined != wantCombined {
		t.Errorf("streamed body = %q; want %q", combined, wantCombined)
	}
}

func TestChatCompletions_FinalMarker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("hello"))
	}))
	defer srv.Close()

	c := New(srv.URL)
	msgs := collectMessages(t, c, []byte(`{}`), "key")

	if len(msgs) == 0 {
		t.Fatal("no messages emitted")
	}
	last := msgs[len(msgs)-1]
	if last.Headers["X-Relay-Final"] != "true" {
		t.Errorf("last message X-Relay-Final = %q; want true", last.Headers["X-Relay-Final"])
	}
}

func TestChatCompletions_NetworkError(t *testing.T) {
	c := New("http://127.0.0.1:1") // nothing listening

	out := make(chan *transport.Message, 64)
	ctx := context.Background()
	c.ChatCompletions(ctx, []byte(`{}`), "key", out)

	var msgs []*transport.Message
	for m := range out {
		msgs = append(msgs, m)
	}

	if len(msgs) == 0 {
		t.Fatal("no messages emitted on network error")
	}
	m := msgs[0]
	if m.Headers["X-Relay-Status"] != "502" {
		t.Errorf("X-Relay-Status = %q; want 502", m.Headers["X-Relay-Status"])
	}
	if m.Headers["X-Relay-Final"] != "true" {
		t.Errorf("X-Relay-Final = %q; want true", m.Headers["X-Relay-Final"])
	}
	if !strings.Contains(string(m.Body), "upstream_error") {
		t.Errorf("body missing upstream_error: %s", m.Body)
	}
}

func TestChatCompletions_NoRelayHeadersUpstream(t *testing.T) {
	var captured http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(srv.URL)
	collectMessages(t, c, []byte(`{}`), "key")

	forbidden := []string{"X-Relay-Request-Id", "X-Request-Id", "X-Relay-Route"}
	for _, name := range forbidden {
		if captured.Get(name) != "" {
			t.Errorf("header %q should not reach upstream, got %q", name, captured.Get(name))
		}
	}
	// Verify only Content-Type and Authorization are set.
	for name := range captured {
		switch strings.ToLower(name) {
		case "content-type", "authorization", "user-agent", "content-length", "accept-encoding":
			// expected
		default:
			t.Errorf("unexpected header sent upstream: %q", name)
		}
	}
}

func TestChatCompletions_ContextCancel(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.WriteHeader(200)
		flusher.Flush()
		close(started)
		// hold the connection open
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := New(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan *transport.Message, 64)

	errCh := make(chan error, 1)
	go func() {
		errCh <- c.ChatCompletions(ctx, []byte(`{}`), "key", out)
	}()

	<-started
	cancel()

	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("ChatCompletions did not return after context cancel")
	}

	var msgs []*transport.Message
	for m := range out {
		msgs = append(msgs, m)
	}

	if len(msgs) == 0 {
		t.Fatal("no messages emitted")
	}
	last := msgs[len(msgs)-1]
	if last.Headers["X-Relay-Final"] != "true" {
		t.Errorf("last message X-Relay-Final = %q; want true", last.Headers["X-Relay-Final"])
	}
}
