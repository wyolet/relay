package payloadlog

import (
	"context"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/lifecycle"
)

// A finalized request's Record reaches the sink whole: on the refused queue
// when its request never reached an upstream, on the main queue when it did.
func TestSinkCollector_RoutesByUpstreamReached(t *testing.T) {
	c, sink := enabledCtrl(0)
	hold := parkedSink()
	e := NewEmitter(EmitterOptions{Logger: testLogger()}, hold, sink)
	reg := lifecycle.New()
	reg.RegisterHook(NewPayloadHook(c))
	reg.RegisterCollector(NewSinkCollector(e))
	e.Emit(rec("parked"))
	<-hold.entered

	refused := optedIn("refused-in")
	reg.Finalize(context.Background(), refused, &lifecycle.PostFlightEvent{ErrorKind: "rate_limited"})
	served := optedIn("served-in")
	served.RequestID = "req-served"
	served.MarkUpstreamStart()
	reg.Finalize(context.Background(), served, &lifecycle.PostFlightEvent{Status: 200, ResponseBody: []byte("served-out")})
	notLogged := optedIn("in")
	notLogged.PayloadLog = false
	reg.Finalize(context.Background(), notLogged, &lifecycle.PostFlightEvent{Status: 200})

	if e.QueueDepth() != 1 || e.RefusedQueueDepth() != 1 {
		t.Fatalf("queue depth = %d, refused queue depth = %d, want 1 and 1", e.QueueDepth(), e.RefusedQueueDepth())
	}
	close(hold.release)
	e.Close()

	wantServed := fullRecord("served-in", "served-out")
	wantServed.RequestID = "req-served"
	want := []Record{rec("parked"), *wantServed, *fullRecord("refused-in", "")}
	if diff := cmp.Diff(want, sink.records); diff != "" {
		t.Fatalf("sink records (-want +got):\n%s", diff)
	}
}

// otherHook attaches something that is not a Record under the payload namespace.
type otherHook struct{ v any }

func (otherHook) Name() string { return Namespace }

func (h otherHook) Fill(*lifecycle.Context, *lifecycle.PostFlightEvent) (any, error) {
	return h.v, nil
}

func TestSinkCollector_IgnoresWhatIsNotARecord(t *testing.T) {
	sink := openSink()
	e := NewEmitter(EmitterOptions{Logger: testLogger()}, sink)
	for _, v := range []any{"payload", (*Record)(nil)} {
		reg := lifecycle.New()
		reg.RegisterHook(otherHook{v})
		reg.RegisterCollector(NewSinkCollector(e))
		reg.Finalize(context.Background(), optedIn("in"), &lifecycle.PostFlightEvent{Status: 200})
	}
	e.Close()
	if sizes, _ := sink.result(); !slices.Equal(sizes, nil) {
		t.Fatalf("sink writes = %v, want none", sizes)
	}
}
