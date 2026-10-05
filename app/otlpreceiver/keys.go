package otlpreceiver

// MarkerKind names what was stored for a reported call. The kinds are marked independently, so content that arrives after a call's usage is still stored once.
type MarkerKind string

const (
	MarkerUsage   MarkerKind = "usage"
	MarkerContent MarkerKind = "content"
)

// memMarkLock is the lock the in-memory emulation of the mark script holds. It is never stored.
const memMarkLock = "{otlp}:mark-lock"

// markerKey returns the kv key that marks one kind as stored for a call reported by tenant. The hash tag is the tenant and the trace id: one script call marks every span of a trace, the markers of one call share a slot whichever signal reported it, and no tenant can mark a call for another.
func markerKey(tenant string, kind MarkerKind, c Call) string {
	return "{otlp:" + tenant + ":" + c.TraceID + "}:" + string(kind) + ":" + c.SpanID
}
