package usagelog

import "github.com/wyolet/relay/pkg/lifecycle"

// SinkCollector is the usage janitor: a lifecycle.Collector that reads
// the *Event UsageHook attached under Namespace and pushes it onto the
// bounded Emitter (→ Sink). Read-only on the Context; the Emit is a
// non-blocking channel send, so it never blocks the post-flight goroutine.
//
// This is the "store" half of the produce → attach → store flow. Adding a
// second destination (ClickHouse, OTel) is a matter of the Emitter
// fanning to more Sinks — not a change to this collector or the hook.
type SinkCollector struct {
	emitter *Emitter
}

// NewSinkCollector constructs a collector that emits collected usage
// Events onto e.
func NewSinkCollector(e *Emitter) *SinkCollector {
	return &SinkCollector{emitter: e}
}

// Collect reads the usage Event off lc and emits it. No-op when no usage
// Event was attached (defensive — UsageHook always attaches one).
func (c *SinkCollector) Collect(lc *lifecycle.Context) {
	v, ok := lc.Collected(Namespace)
	if !ok {
		return
	}
	ev, ok := v.(*Event)
	if !ok || ev == nil {
		return
	}
	c.emitter.Emit(*ev)
}
