package anthropic

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
		errCh <- c.Messages(ctx, body, secret, out)
	}()
	var msgs []*transport.Message
	for m := range out {
		msgs = append(msgs, m)
	}
	<-errCh
	return msgs
}

func TestMessages_APIKeyHeader(t *testing.T) {
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Get("x-api-key")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(srv.URL)
	collectMessages(t, c, []byte(`{}`), "sk-ant-test-key")

	if captured != "sk-ant-test-key" {
		t.Errorf("x-api-key header = %q; want %q", captured, "sk-ant-test-key")
	}
}

func TestMessages_AnthropicVersionHeader(t *testing.T) {
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Get("anthropic-version")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(srv.URL)
	collectMessages(t, c, []byte(`{}`), "key")

	if captured != anthropicVersion {
		t.Errorf("anthropic-version header = %q; want %q", captured, anthropicVersion)
	}
}

func TestMessages_BodyForwarded(t *testing.T) {
	body := []byte(`{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"hi"}],"max_tokens":256}`)
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

func TestMessages_PathIsV1Messages(t *testing.T) {
	var capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(srv.URL)
	collectMessages(t, c, []byte(`{}`), "key")

	if capturedPath != "/v1/messages" {
		t.Errorf("path = %q; want /v1/messages", capturedPath)
	}
}

func TestMessages_StatusAndContentTypePropagated(t *testing.T) {
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

func TestMessages_StreamingChunks(t *testing.T) {
	chunks := []string{
		"event: message_start\ndata: {\"type\":\"message_start\"}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
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

func TestMessages_FinalMarker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"type":"message"}`))
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

func TestMessages_NetworkError(t *testing.T) {
	c := New("http://127.0.0.1:1") // nothing listening

	out := make(chan *transport.Message, 64)
	ctx := context.Background()
	c.Messages(ctx, []byte(`{}`), "key", out)

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

func TestMessages_StatusCodePropagation(t *testing.T) {
	for _, code := range []int{400, 401, 429, 500} {
		code := code
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
			}))
			defer srv.Close()
			c := New(srv.URL)
			msgs := collectMessages(t, c, []byte(`{}`), "key")
			if len(msgs) == 0 {
				t.Fatal("no messages")
			}
			got := msgs[0].Headers["X-Relay-Status"]
			want := http.StatusText(code)
			_ = want
			if got != strings.TrimSpace(http.StatusText(code)) {
				// just check it matches the numeric code
			}
			if got != string([]byte{byte('0' + code/100), byte('0' + (code/10)%10), byte('0' + code%10)}) {
				t.Errorf("X-Relay-Status = %q; want %d", got, code)
			}
		})
	}
}

func TestMessages_ContextCancel(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.WriteHeader(200)
		flusher.Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := New(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan *transport.Message, 64)

	errCh := make(chan error, 1)
	go func() {
		errCh <- c.Messages(ctx, []byte(`{}`), "key", out)
	}()

	<-started
	cancel()

	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("Messages did not return after context cancel")
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

// TestMessages_PassthroughAuth verifies that a secret starting with "Bearer "
// is forwarded as Authorization rather than x-api-key, and that RequestExtras
// headers and query string are forwarded verbatim.
func TestMessages_PassthroughAuth(t *testing.T) {
	var (
		capturedAuth      string
		capturedXApp      string
		capturedStainless string
		capturedQuery     string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		capturedXApp = r.Header.Get("X-App")
		capturedStainless = r.Header.Get("X-Stainless-Lang")
		capturedQuery = r.URL.RawQuery
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(srv.URL)
	ctx := WithRequestExtras(context.Background(), RequestExtras{
		RawQuery: "beta=true",
		ExtraHeaders: map[string]string{
			"X-App":            "cli",
			"X-Stainless-Lang": "go",
		},
	})

	out := make(chan *transport.Message, 64)
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.Messages(ctx, []byte(`{}`), "Bearer sk-ant-oauth-token", out)
	}()
	for range out {
	}
	<-errCh

	if capturedAuth != "Bearer sk-ant-oauth-token" {
		t.Errorf("Authorization = %q; want Bearer sk-ant-oauth-token", capturedAuth)
	}
	if capturedXApp != "cli" {
		t.Errorf("X-App = %q; want cli", capturedXApp)
	}
	if capturedStainless != "go" {
		t.Errorf("X-Stainless-Lang = %q; want go", capturedStainless)
	}
	if capturedQuery != "beta=true" {
		t.Errorf("RawQuery = %q; want beta=true", capturedQuery)
	}
}
