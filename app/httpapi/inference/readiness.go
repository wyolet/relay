package inference

import (
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	appcatalog "github.com/wyolet/relay/app/catalog"
)

// notReadyLogInterval spaces the not-ready WARN: a pod restarting under load refuses thousands of requests a second.
const notReadyLogInterval = time.Second

var (
	notReadySinceLog    atomic.Int64
	notReadyLastLogNano atomic.Int64
)

// ReadinessMiddleware short-circuits inbound /v1/* requests with 503 until
// the catalog has built its first snapshot. Until then, lookups against
// the empty zero-value snapshot would look identical to a legitimately
// empty catalog — better to refuse traffic explicitly so the caller
// retries instead of seeing a misleading 404. /healthz is mounted
// outside this middleware so liveness/readiness probes can still report.
// The refusal closes the connection so a keep-alive client re-dials a ready
// pod, and is logged sampled rather than per request.
func ReadinessMiddleware(cat *appcatalog.Catalog) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !cat.IsReady() {
				recordNotReady()
				w.Header().Set("Retry-After", "5")
				w.Header().Set("Connection", "close")
				writeErrorEnvelope(w, http.StatusServiceUnavailable, "server_error", "catalog_unavailable",
					"catalog is still loading; retry shortly")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// recordNotReady counts one refusal and logs at most one WARN per notReadyLogInterval process-wide, carrying the refusals since the previous line.
func recordNotReady() {
	notReadySinceLog.Add(1)
	now := time.Now().UnixNano()
	last := notReadyLastLogNano.Load()
	if now-last < int64(notReadyLogInterval) || !notReadyLastLogNano.CompareAndSwap(last, now) {
		return
	}
	slog.Warn("inference: refusing requests, catalog not ready", "refused", notReadySinceLog.Swap(0))
}
