package inference

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/pkg/kv"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// TestReserveStoreUnavailableAnswersOverloaded pins the response to a hold the store could not answer: the shed's 503 and Retry-After.
func TestReserveStoreUnavailableAnswersOverloaded(t *testing.T) {
	w := httptest.NewRecorder()
	mapRunErr(w, nil, fmt.Errorf("%w: %w", pkgratelimit.ErrUnavailable, fmt.Errorf("%w: %w", kv.ErrUnavailable, context.DeadlineExceeded)))
	assertOverloaded(t, w)
}

// Proxy mode refuses an unchecked reserve the same way, in the inbound shape's own overload form when it declares one.
func TestProxyReserveStoreUnavailableAnswersOverloaded(t *testing.T) {
	unavailable := fmt.Errorf("%w: %w", pkgratelimit.ErrUnavailable, context.DeadlineExceeded)
	w := httptest.NewRecorder()
	mapProxyErr(w, nil, unavailable)
	assertOverloaded(t, w)

	w = httptest.NewRecorder()
	mapProxyErr(w, buildPathRegistry().Spec(adapters.Anthropic), unavailable)
	assertSpecOverloaded(t, w)
}

// A store outage refuses every request; the refusals are counted, so none may log a WARN line of its own.
func TestReserveStoreUnavailableLogsNoWarn(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	w := httptest.NewRecorder()
	mapRunErr(w, nil, fmt.Errorf("%w: %w", pkgratelimit.ErrUnavailable, fmt.Errorf("%w: %w", kv.ErrUnavailable, context.DeadlineExceeded)))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("pipeline status = %d, want 503: %s", w.Code, w.Body)
	}
	pw := httptest.NewRecorder()
	mapProxyErr(pw, nil, fmt.Errorf("%w: %w", pkgratelimit.ErrUnavailable, context.DeadlineExceeded))
	if pw.Code != http.StatusServiceUnavailable {
		t.Fatalf("proxy status = %d, want 503", pw.Code)
	}

	if strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("store-unavailable refusal logged at WARN:\n%s", logs.String())
	}
}
