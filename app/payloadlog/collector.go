package payloadlog

import "github.com/wyolet/relay/pkg/lifecycle"

// SinkCollector is the payload janitor: a lifecycle.Collector that reads
// the *Record attached under Namespace and pushes it onto the bounded
// Emitter. Read-only on the Context; the Emit is a non-blocking channel
// send. No-op when no Record was attached (capture disabled for the
// request).
type SinkCollector struct {
	emitter *Emitter
}

// NewSinkCollector constructs a collector that emits collected Records
// onto e.
func NewSinkCollector(e *Emitter) *SinkCollector {
	return &SinkCollector{emitter: e}
}

func (c *SinkCollector) Collect(lc *lifecycle.Context) {
	v, ok := lc.Collected(Namespace)
	if !ok {
		return
	}
	r, ok := v.(*Record)
	if !ok || r == nil {
		return
	}
	c.emitter.Emit(*r)
}
