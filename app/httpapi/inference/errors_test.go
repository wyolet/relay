package inference

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/pipeline"
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
	// One client reads the delay out of the message text and never looks at Retry-After.
	if !strings.Contains(env.Err.Message, "try again in 3s") {
		t.Fatalf("message must name the retry delay: %q", env.Err.Message)
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

// Every response must declare which side produced it: relay-minted errors
// carry origin "relay"; pass-through responses carry origin "upstream", and
// an upstream cannot spoof the relay claim.
func TestErrorAttribution_OriginHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	writeAPIError(rec, 401, "authentication_error", "bad_key", "invalid relay key")
	if got := rec.Header().Get(HeaderOrigin); got != "relay" {
		t.Fatalf("relay-minted origin: %q", got)
	}

	rec = httptest.NewRecorder()
	src := http.Header{"X-Foo": {"bar"}, HeaderOrigin: {"relay"}} // spoof attempt
	ForwardUpstreamHeaders(rec.Header(), src)
	if got := rec.Header().Get(HeaderOrigin); got != "upstream" {
		t.Fatalf("pass-through origin: %q (spoof must lose)", got)
	}
	if rec.Header().Get("X-Foo") != "bar" {
		t.Fatal("regular upstream headers must still forward")
	}
}

// An upstream failure that exhausted failover is forwarded, not flattened: the
// caller gets the provider's own status, body and backoff headers, with
// X-WR-Upstream-Status and origin "upstream" declaring who produced them.
func TestMapPipelineErr_UpstreamFailure_ForwardsVerbatim(t *testing.T) {
	rec := httptest.NewRecorder()
	body := []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	mapPipelineErr(rec, &pipeline.UpstreamFailureError{
		Status: 429,
		Header: http.Header{
			"Retry-After":                       {"7"},
			"Anthropic-Ratelimit-Unified-Reset": {"1750000000"},
			"X-Request-Id":                      {"req_abc"},
			"Content-Type":                      {"application/json"},
			"Set-Cookie":                        {"session=leak"},
			"Content-Length":                    {"9999"},
		},
		Body: body,
	})

	if rec.Code != 429 {
		t.Fatalf("status: %d, want the upstream's 429", rec.Code)
	}
	if got := rec.Body.String(); got != string(body) {
		t.Fatalf("body must be byte-identical:\n got %q\nwant %q", got, body)
	}
	for k, want := range map[string]string{
		"Retry-After":                       "7",
		"Anthropic-Ratelimit-Unified-Reset": "1750000000",
		"X-Request-Id":                      "req_abc",
		"Content-Type":                      "application/json",
		HeaderUpstreamStatus:                "429",
		HeaderOrigin:                        "upstream",
	} {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("header %s: %q, want %q", k, got, want)
		}
	}
	if got := rec.Header().Get("Set-Cookie"); got != "" {
		t.Errorf("Set-Cookie must not be copied from upstream: %q", got)
	}
	if got := rec.Header().Get("Content-Length"); got != "" {
		t.Errorf("upstream Content-Length must not describe relay's body: %q", got)
	}
}

// An HTTP-date Retry-After is rewritten as integer seconds — several clients
// parse only the integer form and silently drop a date.
func TestMapPipelineErr_UpstreamFailure_RetryAfterDateToSeconds(t *testing.T) {
	rec := httptest.NewRecorder()
	when := time.Now().Add(42 * time.Second).UTC().Format(http.TimeFormat)
	mapPipelineErr(rec, &pipeline.UpstreamFailureError{
		Status: 429,
		Header: http.Header{"Retry-After": {when}},
		Body:   []byte(`{"error":"rate limited"}`),
	})
	secs, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil {
		t.Fatalf("Retry-After not an integer: %q", rec.Header().Get("Retry-After"))
	}
	if secs < 35 || secs > 45 {
		t.Fatalf("Retry-After: %ds, want ~42s from now", secs)
	}
}

func TestMapPipelineErr_UpstreamFailure_ShouldRetry(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		upstream http.Header
		want     string
	}{
		{"client error is not retryable", 400, nil, "false"},
		{"server error is retryable", 503, nil, "true"},
		{"upstream verdict wins", 503, http.Header{"X-Should-Retry": {"false"}}, "false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mapPipelineErr(rec, &pipeline.UpstreamFailureError{
				Status: tc.status, Header: tc.upstream, Body: []byte(`{"error":"nope"}`),
			})
			if rec.Code != tc.status {
				t.Fatalf("status: %d, want %d", rec.Code, tc.status)
			}
			if got := rec.Header().Get("X-Should-Retry"); got != tc.want {
				t.Fatalf("X-Should-Retry: %q, want %q", got, tc.want)
			}
		})
	}
}

// With no upstream body there is nothing to forward, but the status is still
// the provider's verdict — the caller must not get a bodiless response.
func TestMapPipelineErr_UpstreamFailure_EmptyBodyKeepsStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	mapPipelineErr(rec, &pipeline.UpstreamFailureError{Status: 503, Body: []byte("  \n")})
	if rec.Code != 503 {
		t.Fatalf("status: %d, want 503", rec.Code)
	}
	var env httpapi.OpenAIError
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("relay envelope expected: %v (body %q)", err, rec.Body.String())
	}
	if env.Err.Code != "upstream_unavailable" {
		t.Fatalf("code: %q", env.Err.Code)
	}
	if got := rec.Header().Get(HeaderOrigin); got != "relay" {
		t.Fatalf("origin: %q, want relay (the envelope is relay's)", got)
	}
	if got := rec.Header().Get(HeaderUpstreamStatus); got != "503" {
		t.Fatalf("upstream status header: %q", got)
	}
}

// Relay-minted errors carry their own retry verdict so a client never has to
// guess from the status alone.
func TestWriteAPIError_SetsShouldRetry(t *testing.T) {
	for status, want := range map[int]string{429: "true", 500: "true", 502: "true", 503: "true", 401: "false", 404: "false"} {
		rec := httptest.NewRecorder()
		writeAPIError(rec, status, "server_error", "x", "y")
		if got := rec.Header().Get("X-Should-Retry"); got != want {
			t.Errorf("status %d: X-Should-Retry=%q, want %q", status, got, want)
		}
	}
}
