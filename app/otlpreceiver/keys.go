package otlpreceiver

// MarkerKind names what was stored for a reported call. The kinds are marked independently, so content that arrives after a call's usage is still stored once.
type MarkerKind string

const (
	MarkerUsage   MarkerKind = "usage"
	MarkerContent MarkerKind = "content"
)

// memMarkLock is the lock the in-memory emulation of the mark script holds. It is never stored.
const memMarkLock = "{otlp}:mark-lock"

// markerKey returns the kv key that marks one kind as stored for a call. The hash tag is the trace id: one script call marks every span of a trace, and the markers of one call share a slot whichever signal reported it.
func markerKey(kind MarkerKind, c Call) string {
	return "{otlp:" + c.TraceID + "}:" + string(kind) + ":" + c.SpanID
}
