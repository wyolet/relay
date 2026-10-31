package inference

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/pkg/httpheader"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// WriteAPIError emits relay's error envelope. Exported so
// per-shape route packages (app/adapters/<name>/routes.go) can use it
// without depending on shape-specific helpers. Extra slog attrs are
// attached to the warning log only — never to the client envelope.
func WriteAPIError(w http.ResponseWriter, status int, errType, code, msg string, attrs ...any) {
	writeAPIError(w, status, errType, code, msg, attrs...)
}

// Error-attribution headers. Relay's error envelope and an OpenAI-shaped
// upstream's envelope are indistinguishable by body alone, so every response
// declares which side produced it: a 401 with origin "relay" means your
// relay key; origin "upstream" means the provider rejected the upstream key.
const (
	// HeaderOrigin is "relay" when relay minted the response (auth,
	// routing, rate limit, admission — or a relay verdict about an upstream
	// failure), "upstream" when the provider's bytes passed through.
	HeaderOrigin = "X-Wr-Origin"
	// HeaderUpstreamStatus carries the provider's HTTP status on
	// relay-minted errors that report an upstream failure, so the two
	// layers' statuses are never conflated.
	HeaderUpstreamStatus = "X-Wr-Upstream-Status"
)

// writeAPIError is the internal form used by handlers inside this
// package; WriteAPIError is the exported wrapper for adapter packages.
// attrs add structured fields to the log line (e.g. the requested model
// and policy on a routing rejection) without leaking them into the
// client-facing error body.
func writeAPIError(w http.ResponseWriter, status int, errType, code, msg string, attrs ...any) {
	slog.Warn("inference: error response",
		append([]any{"status", status, "type", errType, "code", code, "msg", msg}, attrs...)...,
	)
	writeErrorEnvelope(w, status, errType, code, msg)
}

// writeErrorEnvelope writes relay's error envelope without logging it, for refusals a flood could repeat and that are counted instead.
func writeErrorEnvelope(w http.ResponseWriter, status int, errType, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(HeaderOrigin, "relay")
	setShouldRetry(w, status)
	w.WriteHeader(status)
	env := httpapi.APIError{
		Err:        httpapi.APIErrorBody{Type: errType, Code: code, Message: msg},
		HTTPStatus: status,
	}
	body, _ := json.Marshal(env)
	_, _ = w.Write(body)
}

// mapRouteRefusal answers a route that has no hop. model is the caller-supplied model ref (safe to echo back); the rung, subject and policy that decided are logged.
func mapRouteRefusal(w http.ResponseWriter, r *plan.Refusal, model string) {
	// log-only attrs; the requested model and the deciding rung make a routing rejection diagnosable from logs.
	attrs := []any{"model", model, "rung", string(r.Rung), "subject", r.SubjectID, "policy", r.PolicyID}
	modelMsg := func(format string) string { return fmt.Sprintf(format, model) }
	switch r.Code {
	case plan.CodeModelNotFound:
		writeAPIError(w, http.StatusNotFound, "invalid_request_error", r.Code, modelMsg("model %q not found"), attrs...)
	case plan.CodeModelDisabled:
		writeAPIError(w, http.StatusForbidden, "invalid_request_error", r.Code, modelMsg("model %q is disabled"), attrs...)
	case plan.CodeModelNotAllowed:
		writeAPIError(w, http.StatusForbidden, "invalid_request_error", r.Code, modelMsg("model %q is not allowed: ")+r.Message, attrs...)
	case plan.CodeNoBinding:
		writeAPIError(w, http.StatusServiceUnavailable, "server_error", "no_host_binding", "no enabled host binding for model", attrs...)
	case plan.CodeNoCredentials:
		writeAPIError(w, http.StatusServiceUnavailable, "server_error", "no_keys", modelMsg("no credential you can reach serves %q"), attrs...)
	default:
		writeAPIError(w, http.StatusForbidden, "invalid_request_error", r.Code, r.Error(), attrs...)
	}
}

// routeRefusalKind is the usage ErrorKind of a route refusal, agreeing with the code mapRouteRefusal answers.
func routeRefusalKind(r *plan.Refusal) string {
	switch r.Code {
	case plan.CodeNoBinding:
		return "no_host_binding"
	case plan.CodeNoCredentials:
		return "no_keys"
	}
	return r.Code
}

// setRetryAfter writes a Retry-After header from a bucket-refill duration,
// rounded up to whole seconds with a floor of 1 so an SDK never reads "0"
// as retry-immediately. Zero/negative durations still emit the floor: a 429
// without Retry-After sends well-behaved clients into their fallback
// backoff, which in practice is a hammer (observed: Claude Code at ~10
// retries/s against a naked 429).
// Returns the seconds written so callers can name the same figure in the error message.
func setRetryAfter(w http.ResponseWriter, d time.Duration) int64 {
	secs := int64(1)
	if d > 0 {
		secs = int64((d + time.Second - 1) / time.Second)
	}
	w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
	return secs
}

// retryableStatus reports whether retrying the identical request can plausibly succeed: a transient server-side condition (5xx), a timeout, or a quota that refills. Any other 4xx is the request's own fault and will fail identically.
func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests ||
		status == http.StatusRequestTimeout ||
		status >= 500
}

// setShouldRetry fills in the retry verdict only when nothing has set one — an upstream's own X-Should-Retry is better informed than relay's status heuristic and must survive.
func setShouldRetry(w http.ResponseWriter, status int) {
	if w.Header().Get(httpheader.HeaderShouldRetry) != "" {
		return
	}
	w.Header().Set(httpheader.HeaderShouldRetry, strconv.FormatBool(retryableStatus(status)))
}

// rateLimitMessage appends the retry delay to msg in the one phrasing some clients scrape out of the message text because they never read Retry-After.
func rateLimitMessage(msg string, secs int64) string {
	return fmt.Sprintf("%s; try again in %ds", msg, secs)
}

// callerLimitCode tells a spent budget from a rate limit.
func callerLimitCode(r *limits.Refused) string {
	if r.Counter.Kind == limits.KindBudget {
		return "budget_exceeded"
	}
	return "rate_limit_exceeded"
}

// callerLimitMessage names the rung and policy whose limit refused, never the rule itself.
func callerLimitMessage(r *limits.Refused) string {
	what := "rate limit"
	if r.Counter.Kind == limits.KindBudget {
		what = "budget"
	}
	return fmt.Sprintf("the %s of the %s policy %s is exhausted", what, r.Counter.Rung, r.Counter.PolicyID)
}

// mapPipelineErr translates pipeline sentinels to HTTP responses.
func mapPipelineErr(w http.ResponseWriter, err error) {
	var upstream *pipeline.UpstreamFailureError
	var unreachable *pipeline.UpstreamUnreachableError
	var limited *pipeline.UpstreamLimitedError
	var refused *limits.Refused
	switch {
	case errors.As(err, &limited):
		// Every credential is over an upstream limit or cooling: the caller's own limits had room. The upstream rules are the host's configuration and stay out of the message.
		secs := setRetryAfter(w, limited.RetryAfter)
		writeAPIError(w, http.StatusTooManyRequests, "rate_limit_error", "rate_limit_exceeded",
			fmt.Sprintf("every credential for this model is over its upstream rate limit; try again in %ds", secs))
	case errors.As(err, &refused):
		// One of the caller's own limits or budgets refused before any upstream call: a 429 with the refill (or, for a budget, the next period start) as Retry-After, naming the rung and policy that refused.
		secs := setRetryAfter(w, refused.RetryAfter)
		writeAPIError(w, http.StatusTooManyRequests, "rate_limit_error", callerLimitCode(refused),
			rateLimitMessage(callerLimitMessage(refused), secs), "counter", refused.Counter.ID)
	case errors.As(err, &unreachable):
		// Dial failure against the host — likely a misconfigured baseURL or a
		// down upstream, not a key problem. Surface it distinctly so operators
		// don't chase key/pool config; the dial target stays in the log.
		writeAPIError(w, http.StatusBadGateway, "server_error", "upstream_unreachable",
			fmt.Sprintf("upstream host %q unreachable after %d attempt(s)", unreachable.Host, unreachable.Attempts), "err", err)
	case errors.Is(err, pipeline.ErrNoKeys):
		writeAPIError(w, http.StatusServiceUnavailable, "server_error", "no_keys", "no host keys")
	case errors.As(err, &upstream) && upstream.Status > 0:
		forwardUpstreamFailure(w, upstream)
	case errors.As(err, &upstream):
		writeAPIError(w, http.StatusBadGateway, "server_error", "upstream_unavailable",
			"all upstream keys failed; "+upstream.Error())
	case errors.Is(err, pipeline.ErrAllKeysExhausted):
		writeAPIError(w, http.StatusBadGateway, "server_error", "upstream_unavailable", "all upstream keys failed")
	case errors.Is(err, pipeline.ErrAdapterMissing):
		writeAPIError(w, http.StatusInternalServerError, "server_error", "no_adapter", "adapter missing")
	default:
		// Transport errors embed the request URL; callers know hosts by slug only.
		writeAPIError(w, http.StatusBadGateway, "server_error", "upstream_error", httpheader.SafeUpstreamError("upstream", err), "err", err)
	}
}

// forwardUpstreamFailure relays the last upstream response that exhausted failover, rather than flattening every provider rejection into one 502: clients decide whether and when to retry from the status, Retry-After and the error wording, and a synthetic 502 tells them none of it.
//
// The body goes out verbatim even when the caller speaks a different wire shape than the upstream that produced it. Error envelopes are not translated here: every client relay targets falls back to matching on status plus message text when it doesn't recognise the envelope, so an unmodified provider error carries more signal than a re-shaped one. Envelope translation, if it ever lands, belongs to the per-client profile layer.
func forwardUpstreamFailure(w http.ResponseWriter, e *pipeline.UpstreamFailureError) {
	status := e.Status
	if status < 400 {
		// A retryable non-error status that still exhausted every key is relay's failure to report, not the caller's to act on.
		status = http.StatusBadGateway
	}

	h := w.Header()
	for k, vs := range e.Header {
		key := http.CanonicalHeaderKey(k)
		if !httpheader.Match(key, httpheader.UpstreamErrorAllowlist) {
			continue
		}
		for _, v := range vs {
			h.Add(key, v)
		}
	}
	normalizeRetryAfter(w)
	setShouldRetry(w, status)
	h.Set(HeaderUpstreamStatus, strconv.Itoa(e.Status))

	if len(bytes.TrimSpace(e.Body)) == 0 {
		// Nothing to forward — the status and headers still carry the verdict, so keep them and fill the body with relay's envelope rather than answering bodiless.
		writeAPIError(w, status, "server_error", "upstream_unavailable", "all upstream keys failed; "+e.Error())
		return
	}
	slog.Warn("inference: error response", "status", status, "origin", "upstream", "msg", e.Error())
	h.Set(HeaderOrigin, "upstream")
	w.WriteHeader(status)
	_, _ = w.Write(e.Body)
}

// normalizeRetryAfter rewrites an HTTP-date Retry-After as integer seconds and drops a value that is neither: the header is the one backoff signal clients read, and several parse only the integer form.
func normalizeRetryAfter(w http.ResponseWriter) {
	v := w.Header().Get("Retry-After")
	if v == "" {
		return
	}
	if _, err := strconv.Atoi(v); err == nil {
		return
	}
	if t, err := http.ParseTime(v); err == nil {
		setRetryAfter(w, time.Until(t))
		return
	}
	w.Header().Del("Retry-After")
}

// isHopByHop returns true for headers that mustn't traverse the proxy.
func isHopByHop(k string) bool {
	switch http.CanonicalHeaderKey(k) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailers", "Transfer-Encoding", "Upgrade":
		return true
	}
	return false
}

// ForwardUpstreamHeaders copies src → dst, dropping hop-by-hop, and stamps
// the response origin as "upstream" (Set, not Add — an upstream must not be
// able to spoof a relay-origin claim). The caller is responsible for any
// further adjustments (e.g. clearing Content-Length when the body size will
// change between upstream and client). Exported so adapter packages can use
// it from their own cross-shape handlers.
func ForwardUpstreamHeaders(dst, src http.Header) {
	for k, vs := range src {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
	dst.Set(HeaderOrigin, "upstream")
}

// SplitSSEChunks is exported so adapter packages can use the same SSE
// chunking logic in their cross-shape stream handlers.
func SplitSSEChunks(data []byte, atEOF bool) (advance int, token []byte, err error) {
	return v1.SplitSSEFrames(data, atEOF)
}
