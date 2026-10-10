package adapter_test

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// capturedRequest holds the path and headers of the last request a
// captureServer received.
type capturedRequest struct {
	mu     sync.Mutex
	path   string
	header http.Header
}

func (c *capturedRequest) Path() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.path
}

func (c *capturedRequest) Header(name string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.header.Get(name)
}

// captureServer is an upstream that answers every request 200 with `{}` and
// keeps what the last one carried; it closes on cleanup.
func captureServer(t *testing.T) (*httptest.Server, *capturedRequest) {
	t.Helper()
	got := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.mu.Lock()
		got.path, got.header = r.URL.Path, r.Header.Clone()
		got.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv, got
}
