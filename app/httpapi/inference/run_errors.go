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
		writeStoreUnavailable(w, err)
		return
	}
	mapPipelineErr(w, err)
}

// writeStoreUnavailable answers a request refused because its rate limits could not be checked with the in-flight shed's response: to the caller both are the same transient overload, retried after the same pause.
func writeStoreUnavailable(w http.ResponseWriter, err error) {
	w.Header().Set("Retry-After", httpapi.RetryAfterShed)
	writeAPIError(w, http.StatusTooManyRequests, "rate_limit_error", "overloaded",
		"relay is at capacity; retry shortly", "err", err)
}
