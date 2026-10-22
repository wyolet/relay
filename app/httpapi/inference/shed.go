package inference

import (
	"encoding/json"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"

	"github.com/wyolet/relay/app/httpapi"
)

// shedBody is the 429 envelope for a shed request, marshalled once: under a flood the refusal itself is the hot path.
var shedBody, _ = json.Marshal(httpapi.APIError{
	Err:        httpapi.APIErrorBody{Type: "rate_limit_error", Code: "overloaded", Message: "relay is at capacity; retry shortly"},
	HTTPStatus: http.StatusTooManyRequests,
})

// shedWhenFull refuses an inference request at the route while the pod is at its in-flight cap, before auth or the body read. A native huma middleware rather than a wrapped net/http one so the pass-through path allocates nothing.
func shedWhenFull(adm *httpapi.Admission) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		if !adm.Full() {
			next(ctx)
			return
		}
		httpapi.RecordShed(httpapi.ShedStageEarly)
		_, w := humachi.Unwrap(ctx)
		// The body was never read; closing spares the server draining it to reuse the connection.
		w.Header().Set("Connection", "close")
		writeShed(w)
	}
}

// writeShed writes the shed 429 with the headers writeAPIError would set, without its per-request log line: sheds are logged sampled by httpapi.RecordShed.
func writeShed(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set(HeaderOrigin, "relay")
	h.Set("Retry-After", httpapi.RetryAfterShed)
	setShouldRetry(w, http.StatusTooManyRequests)
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write(shedBody)
}
