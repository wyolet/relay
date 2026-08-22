package usagelog

import (
	"github.com/wyolet/relay/pkg/lifecycle"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// StreamUsageFactory is the streaming counterpart to UsageHook: a
// lifecycle.StreamObserverFactory whose observer harvests the usage Summary
// incrementally as upstream SSE frames arrive (via v1.StreamSummarizer),
// then builds the same usage Event via the shared buildEventWithSummary at
// end-of-stream. Streamed requests therefore collect usage exactly like
// buffered ones — same Event, same Namespace — so echo and the sink both see
// one shape, WITHOUT the observer ever retaining the whole stream. The
// post-flight Hook is too late for a streamed body (it runs after the stream
// is already written); this fills during the stream.
type StreamUsageFactory struct {
	pricer   *Pricer
	instance string
}

// NewStreamUsageFactory constructs the factory. pricer may be nil (events
// stay unpriced); instanceID may be empty (no instance stamp).
func NewStreamUsageFactory(pricer *Pricer, instanceID string) *StreamUsageFactory {
	return &StreamUsageFactory{pricer: pricer, instance: instanceID}
}

func (*StreamUsageFactory) Name() string { return Namespace }

func (f *StreamUsageFactory) NewObserver(lc *lifecycle.Context) lifecycle.StreamObserver {
	return &streamUsageObserver{lc: lc, pricer: f.pricer, instance: f.instance, summ: v1.NewStreamSummarizer(lc.Translator)}
}

// streamUsageObserver harvests usage for one streamed request incrementally:
// it retains only the running canonical stream state + last usage-bearing
// Summary (in summ), never the frames themselves. A streamed response that
// began is a success (status 200) with no error.
type streamUsageObserver struct {
	lc       *lifecycle.Context
	pricer   *Pricer
	instance string
	summ     *v1.StreamSummarizer
}

// Observe feeds the raw upstream frame to the incremental summarizer. The
// frame is one SSE event with the blank-line separator already stripped by
// the dispatch scanner / session framer; StreamSummarizer re-appends it.
func (o *streamUsageObserver) Observe(frame []byte) {
	o.summ.Observe(frame)
}

func (o *streamUsageObserver) Result() (any, error) {
	s := o.summ.Summary()
	out := buildEventWithSummary(o.lc, 200, "", "", s.Tokens, string(s.FinishReason), o.pricer)
	stampInstance(out, o.instance)
	return out, nil
}
