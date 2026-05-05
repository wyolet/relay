package reqid

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func newHandler(fn func(context.Context)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fn(r.Context())
	})
}

func applyMiddleware(base *slog.Logger, h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	Middleware(base)(h).ServeHTTP(w, r)
	return w
}

func TestValidInboundIDPassthrough(t *testing.T) {
	inbound := "0123456789abcdefABCDEF"
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(HeaderInbound, inbound)

	var gotID string
	w := applyMiddleware(slog.Default(), newHandler(func(ctx context.Context) {
		gotID = From(ctx)
	}), r)

	if w.Header().Get(HeaderOutbound) != inbound {
		t.Errorf("outbound header = %q, want %q", w.Header().Get(HeaderOutbound), inbound)
	}
	if gotID != inbound {
		t.Errorf("From(ctx) = %q, want %q", gotID, inbound)
	}
}

func TestMissingHeaderGeneratesULID(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := applyMiddleware(slog.Default(), newHandler(func(context.Context) {}), r)

	id := w.Header().Get(HeaderOutbound)
	if len(id) != 26 {
		t.Errorf("generated ID length = %d, want 26; id=%q", len(id), id)
	}
}

func TestInvalidInboundGeneratesULID(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(HeaderInbound, "bad\x01value")

	w := applyMiddleware(slog.Default(), newHandler(func(context.Context) {}), r)

	id := w.Header().Get(HeaderOutbound)
	if id == "bad\x01value" {
		t.Error("used invalid inbound header verbatim")
	}
	if len(id) != 26 {
		t.Errorf("generated ID length = %d, want 26; id=%q", len(id), id)
	}
}

func TestTooLongInboundGeneratesULID(t *testing.T) {
	inbound := strings.Repeat("a", 129)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(HeaderInbound, inbound)

	w := applyMiddleware(slog.Default(), newHandler(func(context.Context) {}), r)

	id := w.Header().Get(HeaderOutbound)
	if id == inbound {
		t.Error("used too-long inbound header verbatim")
	}
	if len(id) != 26 {
		t.Errorf("generated ID length = %d, want 26; id=%q", len(id), id)
	}
}

func TestEmptyInboundGeneratesULID(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(HeaderInbound, "")

	w := applyMiddleware(slog.Default(), newHandler(func(context.Context) {}), r)

	id := w.Header().Get(HeaderOutbound)
	if len(id) != 26 {
		t.Errorf("generated ID length = %d, want 26; id=%q", len(id), id)
	}
}

func TestGenerateConcurrent(t *testing.T) {
	var sm sync.Map
	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := Generate()
			if _, loaded := sm.LoadOrStore(id, struct{}{}); loaded {
				t.Errorf("duplicate ULID: %s", id)
			}
		}()
	}
	wg.Wait()
}

func TestLoggerIncludesRequestID(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(HeaderInbound, "test-req-id-123")

	applyMiddleware(logger, newHandler(func(ctx context.Context) {
		Logger(ctx).Info("hello")
	}), r)

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("json decode: %v; buf=%q", err, buf.String())
	}
	if record["request_id"] != "test-req-id-123" {
		t.Errorf("request_id attr = %v, want %q", record["request_id"], "test-req-id-123")
	}
}

func TestFromAndLoggerOnEmptyContext(t *testing.T) {
	ctx := context.Background()
	if id := From(ctx); id != "" {
		t.Errorf("From(background) = %q, want empty", id)
	}
	if l := Logger(ctx); l == nil {
		t.Error("Logger(background) returned nil")
	}
}
