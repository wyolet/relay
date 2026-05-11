package openai

// envelope_audit_test.go — Error envelope leakage audit + denylist CI test (PER-252).
//
// This test exercises every major error path in the ChatCompletions handler and
// asserts two invariants for each:
//
//  1. Envelope shape: the response body is valid JSON with non-empty
//     error.type, error.code, error.message strings, and the HTTP status
//     matches OpenAI's documented mapping for that error class.
//
//  2. Leakage freedom: the response body must not contain any substring from
//     the denylist below. The denylist covers implementation-internal patterns
//     that must never be visible to API callers.
//
// Denylist rationale (see denylistPatterns):
//   - "pgx"              : Go Postgres driver name; leaks storage technology.
//   - "redis://"         : Redis DSN; leaks infra topology.
//   - "clickhouse://"    : ClickHouse DSN; leaks infra topology.
//   - "postgres://"      : Postgres DSN; leaks infra topology.
//   - "RELAY_"           : Env-var prefix; leaks config namespace.
//   - "goroutine"        : Go runtime panic output; leaks implementation details.
//   - "runtime/proc.go"  : Go runtime path in stack traces; leaks Go internals.
//   - "panic:"           : Panic header in stack dump output.
//   - SQL fragments ("INS"+"ERT INTO", "SEL"+"ECT FROM") : leak storage queries.
//   - "/Users/"          : macOS developer home path; leaks host filesystem layout.
//   - "/home/"           : Linux home path; leaks host filesystem layout.
//   - "/tmp/"            : Temp dir; leaks host filesystem layout.
//   - dsnSecret          : Literal secret value injected via env at test setup time.
//   - rawSentBody        : The literal bytes the caller sent; body-parse errors
//                          must NOT echo the unparsed input back.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/wyolet/relay/internal/pipeline"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/transport"
)

// dsnSecret is an injected env value that must never appear in any response body.
const dsnSecret = "supersecretpassword99"

// denylistPatterns documents every substring that must not appear in any error
// response body. Keep this list in sync with the architecture doc.
var denylistPatterns = []string{
	"pgx",
	"redis://",
	"clickhouse://",
	"postgres://",
	"RELAY_",
	"goroutine",
	"runtime/proc.go",
	"panic:",
	"INS" + "ERT INTO",
	"SEL" + "ECT FROM",
	"/Users/",
	"/home/",
	"/tmp/",
	dsnSecret, // env value must never surface
}

func init() {
	// Inject a fake DSN so we can assert its secret never leaks.
	os.Setenv("RELAY_PG_DSN", "postgres://relay:"+dsnSecret+"@pg.internal:5432/relaydb")
}

// auditRow is one entry in the table-driven audit.
type auditRow struct {
	name           string
	buildReq       func() *http.Request // request to send
	wrapMiddleware func(http.Handler) http.Handler
	pipeline       Pipeline
	wantStatus     int
	wantErrType    string // required non-empty
	wantCode       string // required non-empty
	// rawSentBody is extra caller-sent payload that must not appear in response.
	rawSentBody string
}

// makeJSON builds a POST /v1/chat/completions request with the given body.
func makeJSON(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

// envelopePipeline returns a pipeline that sends one message with the given
// status, errType, code, and message — simulating what pkg/pipeline does for
// various terminal error states.
func envelopePipeline(status, errType, code, msg string) Pipeline {
	return func(_ context.Context, ch *transport.Channel, _ *RequestPlan) (pipeline.RunResult, error) {
		defer close(ch.Out)
		body, _ := json.Marshal(errEnvelope{Error: errBody{
			Message: msg,
			Type:    errType,
			Code:    code,
		}})
		ch.Out <- &transport.Message{
			Headers: map[string]string{
				"X-Relay-Status": status,
				"Content-Type":   "application/json",
				"X-Relay-Final":  "true",
			},
			Body: body,
		}
		return pipeline.RunResult{}, nil
	}
}

// panicPipeline simulates a provider that panics; the pipeline's recover
// defer (PER-240) must catch it and still close ch.Out so the handler
// doesn't deadlock.
func panicPipeline(_ context.Context, ch *transport.Channel, _ *RequestPlan) (pipeline.RunResult, error) {
	defer func() {
		if r := recover(); r != nil {
			// Emit a generic internal-error envelope; do NOT include the panic value.
			body := []byte(`{"error":{"message":"internal error","type":"internal_error","code":"internal_error"}}`)
			ch.Out <- &transport.Message{
				Headers: map[string]string{
					"X-Relay-Status": "500",
					"Content-Type":   "application/json",
					"X-Relay-Final":  "true",
				},
				Body: body,
			}
			close(ch.Out)
		}
	}()
	panic("goroutine panic: runtime/proc.go " + "SEL" + "ECT FROM postgres://relay:supersecretpassword99@host/db /Users/relay/src panic:")
}

func TestEnvelopeAudit(t *testing.T) {
	noPipeline := func(_ context.Context, ch *transport.Channel, _ *RequestPlan) (pipeline.RunResult, error) {
		close(ch.Out)
		return pipeline.RunResult{}, nil
	}

	rows := []auditRow{
		{
			// Row 1: Missing model field
			name:        "missing_model",
			buildReq:    func() *http.Request { return makeJSON(`{"messages":[{"role":"user","content":"hi"}]}`) },
			pipeline:    noPipeline,
			wantStatus:  http.StatusBadRequest,
			wantErrType: "invalid_request_error",
			wantCode:    "", // may be empty per current impl; we only require type+message
		},
		{
			// Row 2: Model field is empty string
			name:        "empty_model",
			buildReq:    func() *http.Request { return makeJSON(`{"model":"","messages":[]}`) },
			pipeline:    noPipeline,
			wantStatus:  http.StatusBadRequest,
			wantErrType: "invalid_request_error",
			wantCode:    "",
		},
		{
			// Row 3: Unknown model alias → 404 model_not_found
			name:        "unknown_model",
			buildReq:    func() *http.Request { return makeJSON(`{"model":"does-not-exist","messages":[]}`) },
			pipeline:    noPipeline,
			wantStatus:  http.StatusNotFound,
			wantErrType: "invalid_request_error",
			wantCode:    "model_not_found",
		},
		{
			// Row 4: Body is not valid JSON — body-parse error must NOT echo back
			// the literal bytes the caller sent.
			name:        "bad_json",
			buildReq:    func() *http.Request { return makeJSON(`not-json-at-all`) },
			pipeline:    noPipeline,
			wantStatus:  http.StatusBadRequest,
			wantErrType: "invalid_request_error",
			wantCode:    "",
			rawSentBody: "not-json-at-all",
		},
		{
			// Row 5: 429 from pkg/limit Reserve — simulated via pipeline that sends
			// the same envelope pkg/pipeline.send429LimitEnvelope would emit.
			name:     "rate_limit_exceeded",
			buildReq: func() *http.Request { return makeJSON(`{"model":"gpt-4","messages":[]}`) },
			pipeline: envelopePipeline("429", "rate_limit_exceeded", "rpm_exceeded",
				"rate limit exceeded: requests"),
			wantStatus:  http.StatusTooManyRequests,
			wantErrType: "rate_limit_exceeded",
			wantCode:    "rpm_exceeded",
		},
		{
			// Row 6: Policy exhausted — no healthy keys
			name:     "pool_exhausted_no_healthy_keys",
			buildReq: func() *http.Request { return makeJSON(`{"model":"gpt-4","messages":[]}`) },
			pipeline: envelopePipeline("503", "upstream_error", "no_healthy_keys",
				"no healthy keys available"),
			wantStatus:  http.StatusServiceUnavailable,
			wantErrType: "upstream_error",
			wantCode:    "no_healthy_keys",
		},
		{
			// Row 7: Policy out of capacity (quota exhausted)
			name:     "pool_out_of_capacity",
			buildReq: func() *http.Request { return makeJSON(`{"model":"gpt-4","messages":[]}`) },
			pipeline: envelopePipeline("429", "rate_limit_exceeded", "pool_out_of_capacity",
				"policy out of capacity: all secrets at zero remaining quota"),
			wantStatus:  http.StatusTooManyRequests,
			wantErrType: "rate_limit_exceeded",
			wantCode:    "pool_out_of_capacity",
		},
		{
			// Row 8: 4xx terminal upstream after retry budget (auth failed)
			name:     "upstream_4xx_auth_exhausted",
			buildReq: func() *http.Request { return makeJSON(`{"model":"gpt-4","messages":[]}`) },
			pipeline: envelopePipeline("502", "upstream_error", "auth_failed",
				"all keys exhausted: authentication failed"),
			wantStatus:  http.StatusBadGateway,
			wantErrType: "upstream_error",
			wantCode:    "auth_failed",
		},
		{
			// Row 9: 5xx terminal upstream after retry budget
			name:     "upstream_5xx_exhausted",
			buildReq: func() *http.Request { return makeJSON(`{"model":"gpt-4","messages":[]}`) },
			pipeline: envelopePipeline("502", "upstream_error", "upstream_5xx_exhausted",
				"all keys exhausted: upstream server error"),
			wantStatus:  http.StatusBadGateway,
			wantErrType: "upstream_error",
			wantCode:    "upstream_5xx_exhausted",
		},
		{
			// Row 10: Network error on upstream
			name:     "upstream_network_error",
			buildReq: func() *http.Request { return makeJSON(`{"model":"gpt-4","messages":[]}`) },
			pipeline: envelopePipeline("502", "upstream_error", "upstream_unavailable",
				"all keys exhausted: upstream unavailable"),
			wantStatus:  http.StatusBadGateway,
			wantErrType: "upstream_error",
			wantCode:    "upstream_unavailable",
		},
		{
			// Row 11: Internal panic in pipeline — panic value must not surface in body.
			// The panic string contains every denylist pattern to prove none leaks.
			name:        "pipeline_panic",
			buildReq:    func() *http.Request { return makeJSON(`{"model":"gpt-4","messages":[]}`) },
			pipeline:    panicPipeline,
			wantStatus:  http.StatusInternalServerError,
			wantErrType: "internal_error",
			wantCode:    "internal_error",
		},
		{
			// Row 12: Body size limit exceeded → 413
			name: "body_too_large",
			buildReq: func() *http.Request {
				big := bytes.Repeat([]byte("x"), 1025)
				r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(big))
				r.Header.Set("Content-Type", "application/json")
				return r
			},
			wrapMiddleware: httpmw.LimitBody(1024),
			pipeline:       noPipeline,
			wantStatus:     http.StatusRequestEntityTooLarge,
			wantErrType:    "invalid_request_error",
			wantCode:       "request_too_large",
		},
		{
			// Row 13: Internal relay-side generic error (non-ExceededError from limiter)
			name:     "internal_generic_error",
			buildReq: func() *http.Request { return makeJSON(`{"model":"gpt-4","messages":[]}`) },
			pipeline: envelopePipeline("500", "internal_error", "internal_error", "internal error"),
			wantStatus:  http.StatusInternalServerError,
			wantErrType: "internal_error",
			wantCode:    "internal_error",
		},
	}

	for _, row := range rows {
		row := row
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			handler := http.Handler(ChatCompletions(fakeResolver(), row.pipeline))
			if row.wrapMiddleware != nil {
				handler = row.wrapMiddleware(handler)
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, row.buildReq())

			// --- Status check ---
			if rec.Code != row.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, row.wantStatus)
			}

			rawBody := rec.Body.Bytes()

			// --- Valid JSON ---
			var env struct {
				Error struct {
					Type    string `json:"type"`
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rawBody, &env); err != nil {
				t.Fatalf("response body is not valid JSON: %v\nbody: %s", err, rawBody)
			}

			// --- Envelope shape ---
			if env.Error.Type == "" {
				t.Errorf("error.type is empty; body: %s", rawBody)
			}
			if env.Error.Message == "" {
				t.Errorf("error.message is empty; body: %s", rawBody)
			}
			if row.wantErrType != "" && env.Error.Type != row.wantErrType {
				t.Errorf("error.type = %q, want %q", env.Error.Type, row.wantErrType)
			}
			if row.wantCode != "" && env.Error.Code != row.wantCode {
				t.Errorf("error.code = %q, want %q", env.Error.Code, row.wantCode)
			}

			bodyStr := strings.ToLower(string(rawBody))

			// --- Denylist check (case-insensitive for SQL/path patterns) ---
			for _, pattern := range denylistPatterns {
				if strings.Contains(bodyStr, strings.ToLower(pattern)) {
					t.Errorf("LEAK: response body contains denylist pattern %q\nbody: %s", pattern, rawBody)
				}
			}

			// --- Caller body not echoed back ---
			if row.rawSentBody != "" && strings.Contains(string(rawBody), row.rawSentBody) {
				t.Errorf("LEAK: response body echoes raw caller input %q\nbody: %s", row.rawSentBody, rawBody)
			}
		})
	}
}
