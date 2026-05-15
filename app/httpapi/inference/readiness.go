package inference

import (
	"net/http"

	appcatalog "github.com/wyolet/relay/app/catalog"
)

// ReadinessMiddleware short-circuits inbound /v1/* requests with 503 until
// the catalog has built its first snapshot. Until then, lookups against
// the empty zero-value snapshot would look identical to a legitimately
// empty catalog — better to refuse traffic explicitly so the caller
// retries instead of seeing a misleading 404. /healthz is mounted
// outside this middleware so liveness/readiness probes can still report.
func ReadinessMiddleware(cat *appcatalog.Catalog) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !cat.IsReady() {
				w.Header().Set("Retry-After", "5")
				writeAPIError(w, http.StatusServiceUnavailable, "server_error", "catalog_unavailable",
					"catalog is still loading; retry shortly")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
