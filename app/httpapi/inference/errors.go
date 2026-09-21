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
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/pkg/httpheader"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// WriteAPIError emits an OpenAI-shape error envelope. Exported so
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
	HeaderOrigin = "X-WR-Origin"
	// HeaderUpstreamStatus carries the provider's HTTP status on
	// relay-minted errors that report an upstream failure, so the two
	// layers' statuses are never conflated.
	HeaderUpstreamStatus = "X-WR-Upstream-Status"
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
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(HeaderOrigin, "relay")
	setShouldRetry(w, status)
	w.WriteHeader(status)
	env := httpapi.OpenAIError{
		Err:        httpapi.OpenAIErrorInner{Type: errType, Code: code, Message: msg},
		HTTPStatus: status,
	}
	body, _ := json.Marshal(env)
	_, _ = w.Write(body)
}

// mapRoutingErr translates a routing sentinel to a typed HTTP error. model is
// the caller-supplied model ref (safe to echo back); policy is the resolved
// policy id (logged only, never returned to the caller). Either may be "".
func mapRoutingErr(w http.ResponseWriter, err error, model, policy string) {
	// log-only attrs; the requested model + policy make a routing rejection
	// diagnosable from logs without re-deriving them from the request.
	attrs := []any{"model", model, "policy", policy}
	// modelMsg echoes the requested model into the client message when known.
	modelMsg := func(format, fallback string) string {
		if model == "" {
			return fallback
		}
		return fmt.Sprintf(format, model)
	}
	switch {
	case errors.Is(err, routing.ErrModelNotFound):
		writeAPIError(w, http.StatusNotFound, "invalid_request_error", "model_not_found", modelMsg("model %q not found", "model not found"), attrs...)
	case errors.Is(err, routing.ErrModelDisabled):
		writeAPIError(w, http.StatusForbidden, "invalid_request_error", "model_disabled", modelMsg("model %q is disabled", "model is disabled"), attrs...)
	case errors.Is(err, routing.ErrPolicyNotFound):
		writeAPIError(w, http.StatusForbidden, "invalid_request_error", "policy_not_found", "policy not found", attrs...)
	case errors.Is(err, routing.ErrPolicyDisabled):
		writeAPIError(w, http.StatusForbidden, "invalid_request_error", "policy_disabled", "policy is disabled", attrs...)
	case errors.Is(err, routing.ErrPolicyless):
		writeAPIError(w, http.StatusForbidden, "invalid_request_error", "policyless_disabled", "this relay key has no policy attached; policy-less traffic is disabled on this relay", attrs...)
	case errors.Is(err, routing.ErrModelNotInPolicy):
		writeAPIError(w, http.StatusForbidden, "invalid_request_error", "model_not_allowed", modelMsg("model %q is not allowed by this policy", "model is not allowed by this policy"), attrs...)
	case errors.Is(err, routing.ErrNoHostBinding):
		writeAPIError(w, http.StatusServiceUnavailable, "server_error", "no_host_binding", "no enabled host binding for model", attrs...)
	case errors.Is(err, routing.ErrHostNotFound):
		writeAPIError(w, http.StatusInternalServerError, "server_error", "host_not_found", "host referenced by binding not found", attrs...)
	case errors.Is(err, routing.ErrNoKeys):
		writeAPIError(w, http.StatusServiceUnavailable, "server_error", "no_keys", "no host keys available", attrs...)
	default:
		writeAPIError(w, http.StatusInternalServerError, "server_error", "routing_error", err.Error(), attrs...)
	}
}

// routingErrKind maps a routing sentinel to a machine-readable usage
// ErrorKind. Mirrors mapRoutingErr's switch so the usage log and the HTTP
// response agree on what was rejected. The event Status stays 0 (upstream
// never reached); the kind carries the reason.
func routingErrKind(err error) string {
	switch {
	case errors.Is(err, routing.ErrModelNotFound):
		return "model_not_found"
	case errors.Is(err, routing.ErrModelDisabled):
		return "model_disabled"
	case errors.Is(err, routing.ErrPolicyNotFound):
		return "policy_not_found"
	case errors.Is(err, routing.ErrPolicyDisabled):
		return "policy_disabled"
	case errors.Is(err, routing.ErrPolicyless):
		return "policyless"
	case errors.Is(err, routing.ErrModelNotInPolicy):
		return "model_not_allowed"
	case errors.Is(err, routing.ErrNoHostBinding):
		return "no_host_binding"
	case errors.Is(err, routing.ErrHostNotFound):
		return "host_not_found"
	case errors.Is(err, routing.ErrNoKeys):
		return "no_keys"
	default:
		return "routing_error"
	}
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

// rateLimitMessage keeps the limiter's own wording and appends the retry delay in the one phrasing some clients scrape out of the message text because they never read Retry-After.
func rateLimitMessage(err error, secs int64) string {
	return fmt.Sprintf("%s; try again in %ds", err.Error(), secs)
}

// mapPipelineErr translates pipeline sentinels to HTTP responses.
func mapPipelineErr(w http.ResponseWriter, err error) {
	var upstream *pipeline.UpstreamFailureError
	var unreachable *pipeline.UpstreamUnreachableError
	var exceeded *pkgratelimit.ExceededError
	switch {
	case errors.As(err, &exceeded):
		// Relay's own inbound rate limit rejected the request before any
		// upstream call. This MUST be a 429 with Retry-After from the
		// limiter's bucket-refill timing — it previously fell through to the
		// default 502 "upstream_error", blaming a provider that was never
		// contacted and giving clients no backoff signal.
		secs := setRetryAfter(w, exceeded.RetryAfter)
		writeAPIError(w, http.StatusTooManyRequests, "rate_limit_error", "rate_limit_exceeded",
			rateLimitMessage(exceeded, secs))
	case errors.As(err, &unreachable):
		// Dial failure against the host — likely a misconfigured baseURL or a
		// down upstream, not a key problem. Surface it distinctly so operators
		// don't chase key/pool config.
		writeAPIError(w, http.StatusBadGateway, "server_error", "upstream_unreachable", unreachable.Error())
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
		writeAPIError(w, http.StatusBadGateway, "server_error", "upstream_error", err.Error())
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

// MapPipelineErr is the exported form for adapter-side cross-shape handlers
// that drive pipeline.Pipeline.Run directly.
func MapPipelineErr(w http.ResponseWriter, err error) { mapPipelineErr(w, err) }

// SplitSSEChunks is exported so adapter packages can use the same SSE
// chunking logic in their cross-shape stream handlers.
func SplitSSEChunks(data []byte, atEOF bool) (advance int, token []byte, err error) {
	return splitSSEChunks(data, atEOF)
}
