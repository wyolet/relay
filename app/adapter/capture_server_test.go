package adapter_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// captured is what one upstream request carried.
type captured struct {
	Method string
	URI    string
	Header http.Header
	Body   []byte
}

// captureServer is an upstream that answers every request 200 with `{}`; last returns what the most recent one carried.
func captureServer(t testing.TB) (baseURL string, last func() captured) {
	t.Helper()
	var mu sync.Mutex
	var got captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = captured{Method: r.Method, URI: r.RequestURI, Header: r.Header.Clone(), Body: body}
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() captured {
		mu.Lock()
		defer mu.Unlock()
		return got
	}
}
