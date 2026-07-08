package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/pkg/lifecycle"
)

// TestDispatchShed429 verifies that when the in-flight cap is reached the
// pre-flight abort is mapped to a retriable 429 in the OpenAI error envelope
// with a Retry-After header — not the generic 500 other pre-flight errors get.
func TestDispatchShed429(t *testing.T) {
	adm := httpapi.NewAdmission(1)

	// Prime the single slot so the request under test is shed at admission.
	filler := lifecycle.NewContext("filler", "pipeline", time.Now())
	if err := adm.PreFlight(context.Background(), filler, &lifecycle.PreFlightEvent{}); err != nil {
		t.Fatalf("priming acquire: %v", err)
	}

	reg := lifecycle.New()
	reg.RegisterPreFlight(adm.PreFlight)
	reg.RegisterCollector(adm)

	d := Deps{Lifecycle: reg}
	body := []byte(`{"model":"m","stream":false}`)
	r := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", bytes.NewReader(body))
	w := httptest.NewRecorder()

	Dispatch(d, w, r, DispatchInput{Inbound: adapters.OpenAI, Body: body, ModelName: "m"})

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != httpapi.RetryAfterShed {
		t.Fatalf("Retry-After = %q, want %q", got, httpapi.RetryAfterShed)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}

	var env httpapi.OpenAIError
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not the OpenAI error envelope: %v (body=%s)", err, w.Body.String())
	}
	if env.Err.Type != "rate_limit_error" {
		t.Errorf("error type = %q, want rate_limit_error", env.Err.Type)
	}
	if env.Err.Code != "overloaded" {
		t.Errorf("error code = %q, want overloaded", env.Err.Code)
	}
	if env.Err.Message == "" {
		t.Error("error message is empty")
	}
}

// TestDispatchNonShedPreFlightStays500 confirms the shed mapping is type-aware:
// a different pre-flight abort still returns the generic 500, not a 429.
func TestDispatchNonShedPreFlightStays500(t *testing.T) {
	reg := lifecycle.New()
	reg.RegisterPreFlight(func(context.Context, *lifecycle.Context, *lifecycle.PreFlightEvent) error {
		return context.DeadlineExceeded // any non-shed pre-flight error
	})

	d := Deps{Lifecycle: reg}
	body := []byte(`{"model":"m"}`)
	r := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", bytes.NewReader(body))
	w := httptest.NewRecorder()

	Dispatch(d, w, r, DispatchInput{Inbound: adapters.OpenAI, Body: body, ModelName: "m"})

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q, want empty on a non-shed abort", got)
	}
}
