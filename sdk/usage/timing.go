package usage

// UpstreamTiming is the upstream-leg breakdown. All values are
// microseconds elapsed from the request start (Event.Timestamp) — the
// unit lives here, not in the field names. Every mark is anchored to the
// start, never chained, so derive intervals at query time:
//
//	upstream TTFT    = ResponseStart - Start
//	stream body time = ResponseEnd   - ResponseStart
//
// It is a pure wire shape: the public client reports it as StreamTiming
// and pkg/usage.Event embeds it. Lives in sdk/usage so both the
// vendorable client and the server-side event record can name it without
// a cross-module edge.
type UpstreamTiming struct {
	Start         int64 `json:"start"`          // start → handed to upstream
	ResponseStart int64 `json:"response_start"` // start → first byte (TTFT)
	ResponseEnd   int64 `json:"response_end"`   // start → upstream done
}

// ReasoningTiming is the reasoning span. Microseconds elapsed from the
// request start, anchored not chained, same as UpstreamTiming:
//
//	reasoning span = End - Start
type ReasoningTiming struct {
	Start int64 `json:"start"` // start → first reasoning frame
	End   int64 `json:"end"`   // start → last reasoning frame
}
