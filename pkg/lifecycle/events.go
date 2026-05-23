package lifecycle

import "time"

// PreFlightEvent is the snapshot middleware sees before the upstream
// call. Today this is a placeholder — no fields are loadbearing because
// no middleware consumer has materialized yet. As consumers land,
// fields like a mutable request handle or a resolved routing plan
// will live here. Empty struct keeps the signature stable.
//
// When adding fields:
//   - Prefer types that work across all runners (pipeline, proxy, ws,
//     batch). If a field only makes sense for one runner, push it to
//     lc.Metadata instead.
//   - Pointer fields if middleware needs to mutate.
type PreFlightEvent struct{}

// PostFlightEvent is the snapshot observers see after the request has
// completed (success or failure). Pointer-shared across all observers
// in the parallel post-flight chain — see package doc for the
// read-only invariants.
type PostFlightEvent struct {
	// Status is the upstream HTTP status (or 0 if the request never
	// reached upstream — e.g. pre-flight aborted, routing failed).
	Status int

	// Duration is wall-clock time from request entry to post-flight
	// dispatch. Includes upstream latency + relay overhead.
	Duration time.Duration

	// ErrorKind is a short machine-readable category for failures.
	// Empty on success. Examples: "upstream_429", "no_keys",
	// "model_not_found", "stream_aborted".
	ErrorKind string

	// ErrorMessage is the human-readable detail. Optional even when
	// ErrorKind is set; observers should not require it.
	ErrorMessage string

	// ResponseBody is the buffered upstream response bytes. Observers
	// parse what they need (tokens for usage, full body for audit /
	// cache, etc.). Nil when the runner couldn't buffer (e.g. body
	// exceeded a size cap, or the request failed before bytes flowed).
	//
	// READ-ONLY across parallel observers. To transform, copy first
	// via bytes.Clone.
	ResponseBody []byte
}
