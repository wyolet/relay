package inference

import (
	"errors"
	"net/http"

	"github.com/wyolet/relay/app/httpapi"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// mapRunErr answers a failed pipeline run.
func mapRunErr(w http.ResponseWriter, err error) {
	if errors.Is(err, pkgratelimit.ErrUnavailable) {
		writeStoreUnavailable(w)
		return
	}
	mapPipelineErr(w, err)
}

// writeStoreUnavailable answers a request refused because its rate limits could not be checked with the in-flight shed's response: to the caller both are the same transient overload, retried after the same pause. It logs nothing: a store outage refuses every request, and each refusal is counted in relay_kv_errors_total instead.
func writeStoreUnavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", httpapi.RetryAfterShed)
	writeErrorEnvelope(w, http.StatusTooManyRequests, "rate_limit_error", "overloaded",
		"relay is at capacity; retry shortly")
}
