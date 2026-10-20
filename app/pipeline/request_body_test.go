package pipeline_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/pipeline"
)

// A retried attempt must still send the whole body: releasing it is only allowed once a response is accepted.
func TestRetry_SecondAttemptSendsFullBody(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	got := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got[r.Header.Get("Authorization")] = b
		mu.Unlock()
		if r.Header.Get("Authorization") == "Bearer sk-1" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	adp := (&adapter.Spec{
		Name:        "test",
		DefaultPath: "/v1/generate",
		Auth:        adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
	}).Build().PipelineAdapter()

	body := bytes.Repeat([]byte("x"), 256<<10)
	req := &pipeline.Request{
		Body:        body,
		HostBaseURL: srv.URL,
		Adapter:     adp,
		Keys:        []*hostkey.HostKey{makeKey("h1", "sk-1"), makeKey("h2", "sk-2")},
		Policy:      makePolicy(),
	}
	res, err := newPipeline().Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	drainResult(t, res)

	if res.KeyHash != "h2" {
		t.Fatalf("KeyHash = %q, want h2 (failover)", res.KeyHash)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, auth := range []string{"Bearer sk-1", "Bearer sk-2"} {
		if !bytes.Equal(got[auth], body) {
			t.Fatalf("%s: upstream got %d bytes, want %d", auth, len(got[auth]), len(body))
		}
	}
	if req.Body != nil {
		t.Fatal("pipeline still references the request body after the response was accepted")
	}
}
