package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/httpapi"
)

func serveReadiness(h http.Handler) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	return w
}

func TestReadinessMiddleware_NotReadyRefuses(t *testing.T) {
	called := false
	h := ReadinessMiddleware(emptyCatalog())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))

	w := serveReadiness(h)
	if called {
		t.Fatal("next handler ran before the catalog was ready")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", w.Code)
	}
	for k, want := range map[string]string{"Retry-After": "5", "Connection": "close", HeaderOrigin: "relay"} {
		if got := w.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	var env httpapi.APIError
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	if env.Err.Type != "server_error" || env.Err.Code != "catalog_unavailable" {
		t.Errorf("error type/code = %q/%q, want server_error/catalog_unavailable", env.Err.Type, env.Err.Code)
	}
}

// A restart under load refuses thousands of requests a second; they must not each log.
func TestReadinessMiddleware_NotReadyLogIsSampled(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	notReadyLastLogNano.Store(0)
	notReadySinceLog.Store(0)

	h := ReadinessMiddleware(emptyCatalog())(http.NotFoundHandler())
	for range 1000 {
		serveReadiness(h)
	}

	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Fatalf("logged %d WARN lines for 1000 refusals in one interval, want 1:\n%s", n, logs.String())
	}
	if strings.Contains(logs.String(), "inference: error response") {
		t.Errorf("refusal logged a per-request line:\n%s", logs.String())
	}
}

func TestReadinessMiddleware_ReadyPassesThrough(t *testing.T) {
	cat := emptyCatalog()
	if err := cat.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := ReadinessMiddleware(cat)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	w := serveReadiness(h)
	if w.Code != http.StatusTeapot {
		t.Fatalf("status %d, want the next handler's 418", w.Code)
	}
	if got := w.Header().Get("Connection"); got != "" {
		t.Errorf("Connection = %q on a ready pod, want unset", got)
	}
}
