package ollama

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wyolet/relay/pkg/transport"
)

func collectMessages(t *testing.T, c *Client, body []byte) []*transport.Message {
	t.Helper()
	out := make(chan *transport.Message, 64)
	ctx := context.Background()
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.ChatCompletions(ctx, body, "", out)
	}()
	var msgs []*transport.Message
	for m := range out {
		msgs = append(msgs, m)
	}
	<-errCh
	return msgs
}

func TestChatCompletions_NoAuthHeader(t *testing.T) {
	var captured http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(srv.URL)
	collectMessages(t, c, []byte(`{}`))

	if auth := captured.Get("Authorization"); auth != "" {
		t.Errorf("Authorization header should not be sent to Ollama, got %q", auth)
	}
}

func TestChatCompletions_BodyForwarded(t *testing.T) {
	body := []byte(`{"model":"llama3","messages":[]}`)
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(srv.URL)
	collectMessages(t, c, body)

	if string(got) != string(body) {
		t.Errorf("body forwarded = %q; want %q", got, body)
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
	msgs := collectMessages(t, c, []byte(`{}`))

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

	// Verify final marker.
	if len(msgs) == 0 {
		t.Fatal("no messages emitted")
	}
	last := msgs[len(msgs)-1]
	if last.Headers["X-Relay-Final"] != "true" {
		t.Errorf("last message X-Relay-Final = %q; want true", last.Headers["X-Relay-Final"])
	}
}
