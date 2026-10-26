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

func TestInboundIDKeptAsClientIDOnly(t *testing.T) {
	inbound := "0123456789abcdefABCDEF"
	var ids []string
	for range 2 {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set(HeaderInbound, inbound)
		var gotID, gotClient string
		w := applyMiddleware(slog.Default(), newHandler(func(ctx context.Context) {
			gotID, gotClient = From(ctx), ClientID(ctx)
		}), r)
		if gotID == inbound || len(gotID) != 26 {
			t.Fatalf("From(ctx) = %q, want a relay-minted ULID", gotID)
		}
		if w.Header().Get(HeaderOutbound) != gotID {
			t.Errorf("outbound header = %q, want %q", w.Header().Get(HeaderOutbound), gotID)
		}
		if gotClient != inbound || w.Header().Get(HeaderInbound) != inbound {
			t.Errorf("client id = %q, echoed %q, want %q", gotClient, w.Header().Get(HeaderInbound), inbound)
		}
		ids = append(ids, gotID)
	}
	if ids[0] == ids[1] {
		t.Fatalf("two requests with the same %s share request id %q", HeaderInbound, ids[0])
	}
}

func TestWithNewIDReplacesID(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	var parent, child string
	applyMiddleware(slog.Default(), newHandler(func(ctx context.Context) {
		parent = From(ctx)
		child = From(WithNewID(ctx, slog.Default()))
	}), r)
	if child == "" || child == parent {
		t.Fatalf("WithNewID id = %q, parent %q: want a fresh id", child, parent)
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
	if id, _ := record["request_id"].(string); len(id) != 26 {
		t.Errorf("request_id attr = %v, want a ULID", record["request_id"])
	}
	if record["client_request_id"] != "test-req-id-123" {
		t.Errorf("client_request_id attr = %v, want %q", record["client_request_id"], "test-req-id-123")
	}
}

func TestLoggerBuiltOnceOnFirstUse(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	applyMiddleware(slog.Default(), newHandler(func(ctx context.Context) {
		s := stateFrom(ctx)
		if s.logger != nil {
			t.Fatal("logger built before anything asked for it")
		}
		if first := Logger(ctx); first == nil || Logger(ctx) != first {
			t.Fatal("Logger must build the request logger once and keep returning it")
		}
	}), r)
}

// A frame id keeps the connection's client id but, as before, does not log it.
func TestWithNewIDKeepsClientIDUnlogged(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(HeaderInbound, "client-1")

	applyMiddleware(logger, newHandler(func(ctx context.Context) {
		frame := WithNewID(ctx, logger)
		if ClientID(frame) != "client-1" {
			t.Errorf("frame ClientID = %q, want the connection's", ClientID(frame))
		}
		Logger(frame).Info("frame")
	}), r)

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("json decode: %v; buf=%q", err, buf.String())
	}
	if _, ok := record["client_request_id"]; ok {
		t.Errorf("frame logger carries client_request_id: %v", record)
	}
	if id, _ := record["request_id"].(string); len(id) != 26 {
		t.Errorf("request_id attr = %v, want a ULID", record["request_id"])
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
