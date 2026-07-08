package inference

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/routing"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

func TestMapRoutingErr_ModelNotInPolicy_NamesModel(t *testing.T) {
	rec := httptest.NewRecorder()
	mapRoutingErr(rec, routing.ErrModelNotInPolicy, "gpt-4o", "pol_123")

	if rec.Code != 403 {
		t.Fatalf("status: %d", rec.Code)
	}
	var env httpapi.OpenAIError
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Err.Code != "model_not_allowed" {
		t.Errorf("code: %q", env.Err.Code)
	}
	if !strings.Contains(env.Err.Message, `"gpt-4o"`) {
		t.Errorf("message should name the model: %q", env.Err.Message)
	}
	// the policy id is log-only — it must never reach the client body.
	if strings.Contains(rec.Body.String(), "pol_123") {
		t.Errorf("policy id leaked into client body: %s", rec.Body.String())
	}
}

func TestMapRoutingErr_EmptyModel_FallsBackToGeneric(t *testing.T) {
	rec := httptest.NewRecorder()
	mapRoutingErr(rec, routing.ErrModelNotInPolicy, "", "")

	var env httpapi.OpenAIError
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Err.Message != "model is not allowed by this policy" {
		t.Errorf("generic fallback expected, got: %q", env.Err.Message)
	}
}

// Relay's own inbound rate-limit rejection must be a 429 with Retry-After
// from the limiter's refill timing — not the default 502 it used to fall
// into (blaming an upstream that was never called, with no backoff signal).
func TestMapPipelineErr_InboundLimitExceeded_429WithRetryAfter(t *testing.T) {
	rec := httptest.NewRecorder()
	mapPipelineErr(rec, fmt.Errorf("run: %w", &pkgratelimit.ExceededError{
		Rule:       pkgratelimit.Rule{Name: "rpm"},
		RetryAfter: 2300 * time.Millisecond,
	}))

	if rec.Code != 429 {
		t.Fatalf("status: %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "3" {
		t.Fatalf("Retry-After: %q, want %q (2.3s rounds up)", got, "3")
	}
	var env httpapi.OpenAIError
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Err.Type != "rate_limit_error" || env.Err.Code != "rate_limit_exceeded" {
		t.Fatalf("envelope: type=%q code=%q", env.Err.Type, env.Err.Code)
	}
}

// A zero-duration ExceededError still floors Retry-After at 1 — "0" reads
// as retry-immediately to SDKs, which defeats the header's purpose.
func TestSetRetryAfter_FloorsAtOneSecond(t *testing.T) {
	rec := httptest.NewRecorder()
	setRetryAfter(rec, 0)
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After: %q, want %q", got, "1")
	}
}
