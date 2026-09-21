package tokencount

import (
	"context"
	"time"

	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/lifecycle"
)

// MetadataKeySession is the lifecycle Metadata key carrying the caller's conversation id. The request entry resolves it through the client profile — only the profile knows which header its client marks a session with — and records it under this profile-neutral name so observers need no such knowledge.
const MetadataKeySession = "session_id"

// observeTimeout bounds the kv write. The post-flight goroutine is detached from the response, but it must still end.
const observeTimeout = 5 * time.Second

// Observer feeds completed generations back into the Calibrator. It is a lifecycle.Collector, so it runs after the usage producer filled its Event — which is where the upstream's true input-token count already sits, parsed once, for streamed and buffered responses alike.
type Observer struct {
	cal *Calibrator
}

// NewObserver constructs the collector recording into cal.
func NewObserver(cal *Calibrator) *Observer { return &Observer{cal: cal} }

// Collect records this request's (body bytes, input tokens) pair. No-op unless both are known and the request resolved to a model: an unrouted or failed request measures nothing.
//
// The body length is read off the retained inbound reference — never copied, and skipped when the runner only captured a prefix of it, which would read as a request far denser in tokens than it was.
func (o *Observer) Collect(lc *lifecycle.Context) {
	if o == nil || o.cal == nil || lc == nil {
		return
	}
	if lc.ModelID == "" || lc.RequestBodyTruncated || len(lc.RequestBody) == 0 {
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
	input := int(ev.Tokens["input"])
	if input <= 0 {
		return
	}
	session, _ := lc.Metadata[MetadataKeySession].(string)

	ctx, cancel := context.WithTimeout(context.Background(), observeTimeout)
	defer cancel()
	o.cal.Observe(ctx, session, lc.ModelID, len(lc.RequestBody), input)
}
