package inference

import (
	"encoding/json"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/httpapi"
)

// relayOverloaded answers for a shape that declares no overload response of its own, marshalled once: under a flood the refusal itself is the hot path. 503, not 429: the caller is within its limits and relay is the one out of capacity.
var relayOverloaded = adapter.ErrorResponse{
	Status: http.StatusServiceUnavailable,
	Body: func() []byte {
		b, _ := json.Marshal(httpapi.APIError{Err: httpapi.APIErrorBody{Type: "server_error", Code: "overloaded", Message: httpapi.OverloadMessage}})
		return b
	}(),
}

// overloadedFor returns the overload answer for requests that arrived in spec's shape; nil spec gets relay's own.
func overloadedFor(spec *adapter.Spec) *adapter.ErrorResponse {
	if spec != nil && spec.Overloaded != nil {
		return spec.Overloaded
	}
	return &relayOverloaded
}

// shedWhenFull refuses an inference request at the route while the pod is at its in-flight cap, or while kvPoolBusy (nil-safe) reports that its admission reserve would be refused unsent, before auth or the body read. A native huma middleware rather than a wrapped net/http one so the pass-through path allocates nothing. The connection stays open: the server drains a small unread body itself and closes on its own past that, and forcing a close would turn every refusal into a reconnect.
func shedWhenFull(adm *httpapi.Admission, kvPoolBusy func() bool, overloaded *adapter.ErrorResponse) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		switch {
		case adm.Full():
			httpapi.RecordShed(httpapi.ShedStageEarly)
		case kvPoolBusy != nil && kvPoolBusy():
			httpapi.RecordShed(httpapi.ShedStageKVPool)
		default:
			next(ctx)
			return
		}
		_, w := humachi.Unwrap(ctx)
		writeOverloaded(w, overloaded)
	}
}

// writeOverloaded refuses a request for relay's own lack of capacity, with the headers writeAPIError would set and without its per-request log line: sheds are logged sampled by httpapi.RecordShed, store outages counted in relay_kv_errors_total.
func writeOverloaded(w http.ResponseWriter, resp *adapter.ErrorResponse) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set(HeaderOrigin, "relay")
	h.Set("Retry-After", httpapi.RetryAfterShed)
	setShouldRetry(w, resp.Status)
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}
