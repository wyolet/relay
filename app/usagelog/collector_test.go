package usagelog

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/wyolet/relay/pkg/lifecycle"
	sdkusage "github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// One finalized request is one row in the sink, and the caller-facing echo
// reads the same collected event without the operator-only attribution.
func TestSinkCollectorAndEcho(t *testing.T) {
	sink := openSink()
	emitter := NewEmitter(EmitterOptions{Logger: testLogger()}, sink)
	reg := lifecycle.New()
	reg.RegisterHook(NewUsageHook(testPricer(), "relay-0"))
	reg.RegisterCollector(NewSinkCollector(emitter))

	lc := fullContext()
	if got := EchoFromContext(lc); got != nil {
		t.Fatalf("echo before the event is collected = %+v, want nil", got)
	}
	reg.Finalize(context.Background(), lc, &lifecycle.PostFlightEvent{Status: 200})
	refused := lifecycle.NewContext("req-2", "proxy", eventStart)
	reg.Finalize(context.Background(), refused, &lifecycle.PostFlightEvent{Status: 429, ErrorKind: "rate_limit_exceeded"})
	emitter.Close()

	wantRefused := Event{RequestID: "req-2", Source: "proxy", Timestamp: eventStart, Status: 429,
		ErrorKind: "rate_limit_exceeded", Extras: map[string]string{ExtrasKeyInstance: "relay-0"}}
	// The two rows wait in different queues, so their order in the sink is the drain's.
	byID := cmpopts.SortSlices(func(a, b Event) bool { return a.RequestID < b.RequestID })
	if diff := cmp.Diff([]Event{fullEvent(), wantRefused}, sink.events, byID); diff != "" {
		t.Fatalf("sink rows (-want +got):\n%s", diff)
	}

	wantEcho := &v1.RelayUsage{
		RequestID: "req-1", Tokens: sdkusage.Tokens{"input": 1000, "output": 200}, FinishReason: v1.FinishReasonStop,
		Attempts: 2, Streamed: true,
		Timing: &v1.RelayTiming{End: 1_500_000, Upstream: v1.RelayUpstreamTiming{Start: 3000, ResponseStart: 240_000, ResponseEnd: 1_400_000}},
	}
	if diff := cmp.Diff(wantEcho, EchoFromContext(lc)); diff != "" {
		t.Fatalf("echo (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(&v1.RelayUsage{RequestID: "req-2", Timing: &v1.RelayTiming{}}, EchoFromContext(refused)); diff != "" {
		t.Fatalf("echo of a request that never reached upstream (-want +got):\n%s", diff)
	}
}

// otherHook attaches something that is not a usage event under the usage namespace.
type otherHook struct{ v any }

func (otherHook) Name() string { return Namespace }

func (h otherHook) Fill(*lifecycle.Context, *lifecycle.PostFlightEvent) (any, error) {
	return h.v, nil
}

func TestSinkCollectorAndEchoIgnoreWhatIsNotAnEvent(t *testing.T) {
	for name, v := range map[string]any{"nothing collected": nil, "another type": "usage", "nil event": (*Event)(nil)} {
		sink := openSink()
		emitter := NewEmitter(EmitterOptions{Logger: testLogger()}, sink)
		reg := lifecycle.New()
		if v != nil {
			reg.RegisterHook(otherHook{v})
		}
		reg.RegisterCollector(NewSinkCollector(emitter))
		lc := lifecycle.NewContext("req", "pipeline", eventStart)
		reg.Finalize(context.Background(), lc, &lifecycle.PostFlightEvent{Status: 200})
		emitter.Close()
		if len(sink.events) != 0 || EchoFromContext(lc) != nil {
			t.Errorf("%s: sink rows %v, echo %+v; want neither", name, sink.events, EchoFromContext(lc))
		}
	}
}
