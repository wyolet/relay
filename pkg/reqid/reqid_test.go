package reqid

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/oklog/ulid/v2"
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

// The relay always mints its own id. A client's X-Request-Id is kept beside it, echoed and logged, only when it is 1 to 128 printable ASCII bytes; anything else is dropped whole.
func TestMiddlewareInboundID(t *testing.T) {
	seen := map[string]bool{}
	for _, tc := range []struct {
		name    string
		inbound string
		set     bool
		kept    bool
	}{
		{"no header", "", false, false},
		{"empty header", "", true, false},
		{"plain id", "0123456789abcdefABCDEF", true, true},
		{"the same id again", "0123456789abcdefABCDEF", true, true},
		{"spaces and tildes are printable", "a b~c", true, true},
		{"128 bytes", strings.Repeat("a", 128), true, true},
		{"129 bytes", strings.Repeat("a", 129), true, false},
		{"control byte", "bad\x01value", true, false},
		{"unit separator, just below the range", "bad\x1fvalue", true, false},
		{"delete, just above the range", "bad\x7fvalue", true, false},
		{"non-ASCII", "réq", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.set {
				r.Header.Set(HeaderInbound, tc.inbound)
			}
			var id, clientID string
			w := applyMiddleware(slog.New(slog.NewJSONHandler(&buf, nil)), newHandler(func(ctx context.Context) {
				id, clientID = From(ctx), ClientID(ctx)
				Logger(ctx).Info("hello")
			}), r)

			if _, err := ulid.ParseStrict(id); err != nil || id == tc.inbound || seen[id] {
				t.Fatalf("From(ctx) = %q, want a fresh relay-minted ULID (%v)", id, err)
			}
			seen[id] = true
			wantClient := ""
			if tc.kept {
				wantClient = tc.inbound
			}
			echoed := w.Header().Values(HeaderInbound)
			if clientID != wantClient || (tc.kept && !slices.Equal(echoed, []string{tc.inbound})) || (!tc.kept && echoed != nil) {
				t.Errorf("client id = %q, echoed %q; want %q", clientID, echoed, wantClient)
			}
			if got := w.Header().Get(HeaderOutbound); got != id {
				t.Errorf("%s = %q, want the minted id %q", HeaderOutbound, got, id)
			}

			var record map[string]any
			if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
				t.Fatalf("log line: %v (%q)", err, buf.String())
			}
			logged, isLogged := record["client_request_id"]
			if record["request_id"] != id || isLogged != tc.kept || (tc.kept && logged != tc.inbound) {
				t.Errorf("log line = %v, want request_id %q and client_request_id only for a kept id", record, id)
			}
		})
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
