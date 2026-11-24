package inference

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/plan"
)

// Every route refusal answers with its status, wire code, class and a message naming the model the caller asked for.
func TestMapRouteRefusal(t *testing.T) {
	const notAllowed, unreachable = string(plan.RefusalModelNotAllowed), string(plan.RefusalHostNotReachable)
	tests := []struct {
		name    string
		refusal plan.Refusal
		status  int
		want    httpapi.APIErrorBody
	}{
		{"model not found", plan.Refusal{Code: plan.CodeModelNotFound, Message: "m"}, 404,
			httpapi.APIErrorBody{Type: "invalid_request_error", Code: "model_not_found", Refusal: unreachable, Message: `model "gpt-4o" not found`}},
		{"model disabled", plan.Refusal{Code: plan.CodeModelDisabled, Message: "m"}, 403,
			httpapi.APIErrorBody{Type: "invalid_request_error", Code: "model_disabled", Refusal: unreachable, Message: `model "gpt-4o" is disabled`}},
		// Named as a caller limit's 429 names it: the rung, then the policy.
		{"model not allowed by a policy", plan.Refusal{Code: plan.CodeModelNotAllowed, Rung: limits.RungProject, PolicyID: "pol_123", Message: "the plan's reason"}, 403,
			httpapi.APIErrorBody{Type: "invalid_request_error", Code: "model_not_allowed", Refusal: notAllowed, Message: `model "gpt-4o" is not allowed by the project policy pol_123`}},
		// A refusal no policy made (a deprecated model nobody opted into) keeps the plan's reason.
		{"model not allowed without a policy", plan.Refusal{Code: plan.CodeModelNotAllowed, Message: "deprecated models need a policy"}, 403,
			httpapi.APIErrorBody{Type: "invalid_request_error", Code: "model_not_allowed", Refusal: notAllowed, Message: `model "gpt-4o" is not allowed: deprecated models need a policy`}},
		{"no binding", plan.Refusal{Code: plan.CodeNoBinding, Message: "m"}, 503,
			httpapi.APIErrorBody{Type: "server_error", Code: "no_host_binding", Refusal: unreachable, Message: "no enabled host binding for model"}},
		{"no credentials", plan.Refusal{Code: plan.CodeNoCredentials, Message: "m"}, 503,
			httpapi.APIErrorBody{Type: "server_error", Code: "no_keys", Refusal: unreachable, Message: `no credential you can reach serves "gpt-4o"`}},
	}
	// Codes without a message of their own answer 403 with the plan's.
	for _, code := range []string{plan.CodePolicyDisabled, plan.CodeUnpricedUnderBudget, plan.CodeAccountClosed} {
		r := plan.Refusal{Code: code, Message: "the plan's reason"}
		tests = append(tests, struct {
			name    string
			refusal plan.Refusal
			status  int
			want    httpapi.APIErrorBody
		}{code, r, 403, httpapi.APIErrorBody{Type: "invalid_request_error", Code: code, Refusal: notAllowed, Message: r.Error()}})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mapRouteRefusal(rec, &tt.refusal, "gpt-4o")
			wantAnswer(t, rec, tt.status, tt.want, map[string]string{HeaderOrigin: "relay", "Retry-After": ""})
		})
	}
}

// Refusals the pipeline makes before any upstream call are classed and carry their backoff; a failure it cannot name is a 502 that discloses no address.
func TestMapPipelineErr_RelayAnswers(t *testing.T) {
	budgetCounter := limits.Counter{Kind: limits.KindBudget, Rung: limits.RungProject, PolicyID: "pol_p", ID: "b:x"}
	ruleCounter := limits.Counter{Kind: limits.KindRule, Rung: limits.RungTeam, PolicyID: "pol_team", ID: "r:x"}
	const noCapacity, unreachable = string(plan.RefusalNoCapacity), string(plan.RefusalHostNotReachable)
	tests := []struct {
		name       string
		err        error
		status     int
		retryAfter string
		want       httpapi.APIErrorBody
	}{
		// 2.3s rounds up; one client reads the delay out of the message and never looks at Retry-After.
		{"caller rate limit", fmt.Errorf("run: %w", &pipeline.CallerLimitedError{Refused: &limits.Refused{Counter: &ruleCounter, RetryAfter: 2300 * time.Millisecond}}), 429, "3",
			httpapi.APIErrorBody{Type: "rate_limit_error", Code: "rate_limit_exceeded", Refusal: noCapacity, Message: "the rate limit of the team policy pol_team is exhausted; try again in 3s"}},
		{"caller budget", &pipeline.CallerLimitedError{Refused: &limits.Refused{Counter: &budgetCounter, RetryAfter: 14 * time.Hour}}, 429, "50400",
			httpapi.APIErrorBody{Type: "rate_limit_error", Code: "budget_exceeded", Refusal: noCapacity, Message: "the budget of the project policy pol_p is exhausted; try again in 50400s"}},
		// A zero delay still answers 1: "0" reads as retry-immediately.
		{"caller rate limit with no delay", &pipeline.CallerLimitedError{Refused: &limits.Refused{Counter: &ruleCounter}}, 429, "1",
			httpapi.APIErrorBody{Type: "rate_limit_error", Code: "rate_limit_exceeded", Refusal: noCapacity, Message: "the rate limit of the team policy pol_team is exhausted; try again in 1s"}},
		// The upstream rules are the host's configuration and stay out of the message.
		{"every credential over an upstream limit", &pipeline.UpstreamLimitedError{RetryAfter: 11500 * time.Millisecond}, 429, "12",
			httpapi.APIErrorBody{Type: "rate_limit_error", Code: "rate_limit_exceeded", Refusal: noCapacity, Message: "every credential for this model is over its upstream rate limit; try again in 12s"}},
		{"every key out of rotation", keypool.ErrNoHealthyKeys, 503, "",
			httpapi.APIErrorBody{Type: "server_error", Code: "no_healthy_keys", Refusal: noCapacity, Message: "every host key for this model is out of rotation after failing; retry later"}},
		{"no key for the shape", pipeline.ErrNoKeys, 503, "",
			httpapi.APIErrorBody{Type: "server_error", Code: "no_keys", Refusal: unreachable, Message: "no host keys"}},
		// The dial target stays in the error; the caller is told the host by slug.
		{"no host connected", &pipeline.UpstreamUnreachableError{Host: "h", Attempts: 2, Cause: errors.New("dial tcp 10.0.0.7:443"), NoneConnected: true}, 502, "",
			httpapi.APIErrorBody{Type: "server_error", Code: "upstream_unreachable", Refusal: unreachable, Message: `upstream host "h" unreachable after 2 attempt(s)`}},
		{"last host down after another connected", &pipeline.UpstreamUnreachableError{Host: "h", Attempts: 2, Cause: errors.New("dial")}, 502, "",
			httpapi.APIErrorBody{Type: "server_error", Code: "upstream_unreachable", Message: `upstream host "h" unreachable after 2 attempt(s)`}},
		// The path template stays in the error; the caller is told the host and the key it lacks.
		{"host path placeholder unfilled", &pipeline.PathPlaceholderError{Host: "h", Path: "/v1/projects/{project}/chat", Key: "project"}, 502, "",
			httpapi.APIErrorBody{Type: "server_error", Code: "host_misconfigured", Refusal: unreachable, Message: `upstream host "h" is misconfigured: its backend sets no value for the path placeholder {project}`}},
		{"keys exhausted without a response", pipeline.ErrAllKeysExhausted, 502, "",
			httpapi.APIErrorBody{Type: "server_error", Code: "upstream_unavailable", Message: "all upstream keys failed"}},
		{"upstream failure without a status", &pipeline.UpstreamFailureError{Cause: pipeline.ErrAllKeysExhausted}, 502, "",
			httpapi.APIErrorBody{Type: "server_error", Code: "upstream_unavailable", Message: "all upstream keys failed; upstream returned 0: (empty body)"}},
		{"adapter missing", pipeline.ErrAdapterMissing, 500, "",
			httpapi.APIErrorBody{Type: "server_error", Code: "no_adapter", Message: "adapter missing"}},
		{"transport error", errors.New("reset by peer"), 502, "",
			httpapi.APIErrorBody{Type: "server_error", Code: "upstream_error", Message: "upstream: reset by peer"}},
		// Go's transport errors embed the request URL.
		{"transport error naming the upstream URL", &url.Error{Op: "Post", URL: "https://10.0.0.7:8443/v1/messages", Err: errors.New("tls: failed to verify certificate")}, 502, "",
			httpapi.APIErrorBody{Type: "server_error", Code: "upstream_error", Message: "upstream: upstream connection failed"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mapPipelineErr(rec, tt.err)
			wantAnswer(t, rec, tt.status, tt.want, map[string]string{
				HeaderOrigin: "relay", "Retry-After": tt.retryAfter, HeaderUpstreamStatus: "",
				"X-Should-Retry": fmt.Sprint(tt.status == 429 || tt.status >= 500),
			})
			if got := pipeline.RefusalOf(tt.err); string(got) != tt.want.Refusal {
				t.Errorf("RefusalOf = %q, want %q", got, tt.want.Refusal)
			}
		})
	}
}

// An upstream failure that exhausted failover is forwarded, not flattened: the provider's status, body and backoff headers, with X-WR-Upstream-Status and the origin declaring who produced them.
func TestMapPipelineErr_ForwardsUpstreamFailure(t *testing.T) {
	const body = `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`
	in42s := time.Now().Add(42 * time.Second).UTC().Format(http.TimeFormat)
	tests := []struct {
		name       string
		err        pipeline.UpstreamFailureError
		wantStatus int
		wantBody   string // empty: relay's upstream_unavailable envelope
		wantHeader http.Header
	}{
		{
			name: "status, body and allowlisted headers",
			err: pipeline.UpstreamFailureError{Status: 429, Body: []byte(body), Header: http.Header{
				"Retry-After":                       {"7"},
				"anthropic-ratelimit-unified-reset": {"1750000000"},
				"X-Ratelimit-Remaining-Requests":    {"0", "1"},
				"X-Request-Id":                      {"req_abc"},
				"Content-Type":                      {"application/json"},
				"Set-Cookie":                        {"session=leak"},
				"Content-Length":                    {"9999"},
				"Connection":                        {"close"},
				HeaderOrigin:                        {"relay"},
			}},
			wantStatus: 429, wantBody: body,
			wantHeader: http.Header{
				"Retry-After":                       {"7"},
				"Anthropic-Ratelimit-Unified-Reset": {"1750000000"},
				"X-Ratelimit-Remaining-Requests":    {"0", "1"},
				"X-Request-Id":                      {"req_abc"},
				"Content-Type":                      {"application/json"},
				"X-Should-Retry":                    {"true"},
				HeaderUpstreamStatus:                {"429"},
				HeaderOrigin:                        {"upstream"},
			},
		},
		{
			name:       "client error is not retryable",
			err:        pipeline.UpstreamFailureError{Status: 400, Body: []byte(body)},
			wantStatus: 400, wantBody: body,
			wantHeader: http.Header{"X-Should-Retry": {"false"}, HeaderUpstreamStatus: {"400"}, HeaderOrigin: {"upstream"}},
		},
		{
			name:       "the upstream's own retry verdict wins",
			err:        pipeline.UpstreamFailureError{Status: 503, Body: []byte(body), Header: http.Header{"X-Should-Retry": {"false"}}},
			wantStatus: 503, wantBody: body,
			wantHeader: http.Header{"X-Should-Retry": {"false"}, HeaderUpstreamStatus: {"503"}, HeaderOrigin: {"upstream"}},
		},
		{
			// Several clients parse only the integer form.
			name:       "an HTTP-date Retry-After becomes seconds",
			err:        pipeline.UpstreamFailureError{Status: 429, Body: []byte(body), Header: http.Header{"Retry-After": {in42s}}},
			wantStatus: 429, wantBody: body,
			wantHeader: http.Header{"Retry-After": {"42"}, "X-Should-Retry": {"true"}, HeaderUpstreamStatus: {"429"}, HeaderOrigin: {"upstream"}},
		},
		{
			name:       "an unreadable Retry-After is dropped",
			err:        pipeline.UpstreamFailureError{Status: 429, Body: []byte(body), Header: http.Header{"Retry-After": {"soon"}}},
			wantStatus: 429, wantBody: body,
			wantHeader: http.Header{"X-Should-Retry": {"true"}, HeaderUpstreamStatus: {"429"}, HeaderOrigin: {"upstream"}},
		},
		{
			// A retryable non-error status that exhausted every key is relay's failure to report.
			name:       "a status below 400 answers 502",
			err:        pipeline.UpstreamFailureError{Status: 302, Body: []byte(body)},
			wantStatus: 502, wantBody: body,
			wantHeader: http.Header{"X-Should-Retry": {"true"}, HeaderUpstreamStatus: {"302"}, HeaderOrigin: {"upstream"}},
		},
		{
			// Nothing to forward, but the status is still the provider's verdict and the caller must not get a bodiless response.
			name:       "a blank body gets relay's envelope under the upstream status",
			err:        pipeline.UpstreamFailureError{Status: 503, Body: []byte("  \n"), Header: http.Header{"Retry-After": {"9"}}},
			wantStatus: 503,
			wantHeader: http.Header{"Retry-After": {"9"}, "X-Should-Retry": {"true"}, HeaderUpstreamStatus: {"503"}, HeaderOrigin: {"relay"}, "Content-Type": {"application/json"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mapPipelineErr(rec, &tt.err)
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if diff := cmp.Diff(tt.wantHeader, rec.Header()); diff != "" {
				t.Errorf("headers (-want +got):\n%s", diff)
			}
			if tt.wantBody != "" {
				if got := rec.Body.String(); got != tt.wantBody {
					t.Errorf("body = %q, want the upstream's bytes", got)
				}
				return
			}
			want := httpapi.APIErrorBody{Type: "server_error", Code: "upstream_unavailable", Message: "all upstream keys failed; upstream returned 503: (empty body)"}
			if diff := cmp.Diff(want, envelopeOf(t, rec)); diff != "" {
				t.Errorf("envelope (-want +got):\n%s", diff)
			}
		})
	}
}

// Relay-minted errors carry their own retry verdict so a client never has to guess from the status alone.
func TestWriteAPIError_ShouldRetry(t *testing.T) {
	for status, want := range map[int]string{408: "true", 429: "true", 500: "true", 502: "true", 503: "true", 400: "false", 401: "false", 404: "false", 499: "false"} {
		rec := httptest.NewRecorder()
		WriteAPIError(rec, status, "server_error", "x", "y")
		wantAnswer(t, rec, status, httpapi.APIErrorBody{Type: "server_error", Code: "x", Message: "y"}, map[string]string{"X-Should-Retry": want, HeaderOrigin: "relay"})
	}
}

// Pass-through headers keep every value, lose the hop-by-hop ones, and cannot claim relay produced the response.
func TestForwardUpstreamHeaders(t *testing.T) {
	dst := http.Header{"X-Existing": {"kept"}}
	ForwardUpstreamHeaders(dst, http.Header{
		"X-Foo":              {"a", "b"},
		"Content-Type":       {"text/event-stream"},
		HeaderOrigin:         {"relay"},
		"Connection":         {"close"},
		"keep-alive":         {"timeout=5"},
		"Transfer-Encoding":  {"chunked"},
		"Upgrade":            {"h2c"},
		"Te":                 {"trailers"},
		"Trailer":            {"X-Checksum"},
		"Proxy-Authenticate": {"Basic"},
	})
	want := http.Header{"X-Existing": {"kept"}, "X-Foo": {"a", "b"}, "Content-Type": {"text/event-stream"}, HeaderOrigin: {"upstream"}}
	if diff := cmp.Diff(want, dst); diff != "" {
		t.Errorf("forwarded headers (-want +got):\n%s", diff)
	}
}
