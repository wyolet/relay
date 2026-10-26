package tokencount

import (
	"context"

	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/lifecycle"
)

// MetadataKeySession is the lifecycle Metadata key carrying the caller's conversation id. The request entry resolves it through the client profile — only the profile knows which header its client marks a session with — and records it under this profile-neutral name so observers need no such knowledge.
const MetadataKeySession = "session_id"

// Observer feeds completed generations back into the Calibrator. It is a lifecycle.Collector, so it runs after the usage producer filled its Event — which is where the upstream's true input-token count already sits, parsed once, for streamed and buffered responses alike.
type Observer struct {
	cal *Calibrator
}

// NewObserver constructs the collector recording into cal.
func NewObserver(cal *Calibrator) *Observer { return &Observer{cal: cal} }

// Collect records this request's (body bytes, prompt tokens) pair. No-op unless both are known and the request resolved to a model: an unrouted or failed request measures nothing.
func (o *Observer) Collect(lc *lifecycle.Context) {
	if o == nil || o.cal == nil || lc == nil {
		return
	}
	if lc.ModelID == "" || lc.RequestBytes == 0 {
		return
	}
	v, ok := lc.Collected(usagelog.Namespace)
	if !ok {
		return
	}
	ev, ok := v.(*usagelog.Event)
	if !ok || ev == nil {
		return
	}
	// The body carries the whole prompt, cached prefix included; "input" alone is only the uncached remainder on a cache hit.
	prompt := int(ev.Tokens.PromptTokens())
	if prompt <= 0 {
		return
	}
	session, _ := lc.Metadata[MetadataKeySession].(string)

	o.cal.Observe(context.Background(), session, lc.ModelID, lc.RequestBytes, prompt)
}
