package usagelog

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/usage"
	sdkusage "github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// canonicalStream is a canonical SSE body ending in a completed event that carries usage.
func canonicalStream(t *testing.T) []byte {
	t.Helper()
	completed, err := json.Marshal(v1.GenerationCompletedEvent{
		ID: "resp_1", Status: "completed", FinishReason: v1.FinishReasonStop, ServiceTier: "priority",
		Usage: sdkusage.Tokens{"input": 1000, "output": 200},
	})
	if err != nil {
		t.Fatal(err)
	}
	var body []byte
	for _, f := range []v1.SSEFrame{
		{Event: v1.EventGenerationCreated, Data: []byte(`{}`)},
		{Event: v1.EventItemDelta, Data: []byte(`{"delta":"hi"}`)},
		{Event: v1.EventGenerationCompleted, Data: completed},
	} {
		body = append(body, f.Bytes()...)
	}
	return body
}

// A streamed request lands the event a buffered one does, built from the
// Summary the session harvested, however the bytes were chunked on the way in.
func TestStreamUsageObserver_EventFromTheSessionSummary(t *testing.T) {
	body := canonicalStream(t)
	want := fullEvent()
	for name, chunk := range map[string]int{"whole": len(body), "7 bytes": 7, "1 byte": 1} {
		t.Run(name, func(t *testing.T) {
			reg := lifecycle.New()
			reg.RegisterStreamObserver(NewStreamUsageFactory(testPricer(), "relay-0"))
			lc := fullContext()
			lc.SetResponseSummary(v1.Summary{}) // the stream's own Summary is the source
			lc.Translator = stubTranslator{}
			sess := reg.NewStreamSession(lc)
			for rest := body; len(rest) > 0; {
				n := min(chunk, len(rest))
				if _, err := sess.Write(rest[:n]); err != nil {
					t.Fatalf("Write: %v", err)
				}
				rest = rest[n:]
			}
			sess.Finish()

			got, ok := lc.Collected(Namespace)
			if !ok {
				t.Fatal("no usage event collected")
			}
			if diff := cmp.Diff(want, *got.(*Event)); diff != "" {
				t.Fatalf("event (-want +got):\n%s", diff)
			}
			limitTokens, ok := lc.StreamTokens()
			if diff := cmp.Diff(map[string]int64{"input": 1000, "output": 200}, limitTokens); diff != "" || !ok {
				t.Fatalf("rate-limit tokens set=%v (-want +got):\n%s", ok, diff)
			}
		})
	}
}

// A streamed response records the status the runner stamped, not a fixed 200,
// and nothing the observer is fed reaches the event.
func TestStreamUsageObserver_RecordsTheStampedStatus(t *testing.T) {
	factory := NewStreamUsageFactory(nil, "")
	if factory.Name() != Namespace {
		t.Fatalf("Name = %q, want %q", factory.Name(), Namespace)
	}
	for _, tc := range []struct {
		stamped, wantStatus int
		wantKind, wantMsg   string
	}{
		{0, 200, "", ""},
		{200, 200, "", ""},
		{400, 400, usage.ErrorKindUpstream, "upstream returned 400"},
		{529, 529, usage.ErrorKindUpstream, "upstream returned 529"},
	} {
		lc := lifecycle.NewContext("req", "pipeline", eventStart)
		lc.ResponseStatus = tc.stamped
		obs := factory.NewObserver(lc)
		obs.Observe([]byte(`{"error":{"message":"bad request"}}`))
		v, err := obs.Result()
		if err != nil {
			t.Fatalf("stamped %d: Result: %v", tc.stamped, err)
		}
		want := Event{RequestID: "req", Source: "pipeline", Timestamp: eventStart, Status: tc.wantStatus, ErrorKind: tc.wantKind, ErrorMessage: tc.wantMsg}
		if diff := cmp.Diff(want, *v.(*Event)); diff != "" {
			t.Errorf("stamped %d (-want +got):\n%s", tc.stamped, diff)
		}
	}
}
