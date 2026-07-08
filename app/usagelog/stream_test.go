package usagelog

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/pkg/lifecycle"
	sdkusage "github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// recordedStream is a canonical SSE body split into the frames a dispatch
// scanner / stream-session framer hands the observer (separator stripped).
// Feeding it frame-by-frame must reproduce parsing the whole body once.
func recordedStream(t *testing.T) [][]byte {
	t.Helper()
	completed, err := json.Marshal(v1.GenerationCompletedEvent{
		ID:           "resp_1",
		Status:       "completed",
		Usage:        sdkusage.Tokens{"input": 1000, "output": 200, "cache_read": 50},
		FinishReason: v1.FinishReasonStop,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := [][]byte{
		v1.SSEFrame{Event: v1.EventGenerationCreated, Data: []byte(`{}`)}.Bytes(),
		v1.SSEFrame{Event: v1.EventItemDelta, Data: []byte(`{"delta":"hi"}`)}.Bytes(),
		v1.SSEFrame{Event: v1.EventGenerationCompleted, Data: completed}.Bytes(),
	}
	frames := make([][]byte, 0, len(raw))
	for _, f := range raw {
		frames = append(frames, bytes.TrimRight(f, "\n"))
	}
	return frames
}

// oldStreamUsageResult is the PRE-change observer behavior, captured here
// before deletion: accumulate every frame (re-appending the separator the
// scanner stripped) into one unbounded buffer, then build the Event by
// re-parsing the whole buffer via buildEvent (which runs v1.ExtractSummary).
func oldStreamUsageResult(lc *lifecycle.Context, pricer *Pricer, frames [][]byte) *Event {
	var buf bytes.Buffer
	for _, f := range frames {
		buf.Write(f)
		buf.WriteString("\n\n")
	}
	return buildEvent(lc, 200, "", "", buf.Bytes(), pricer)
}

// The incremental observer must land the SAME Event as the old
// accumulate-then-parse path — the whole reason it's allowed to stop
// retaining the stream.
func TestStreamUsageObserver_ParityWithAccumulate(t *testing.T) {
	frames := recordedStream(t)
	pricer := testPricer(map[string]*pricing.Pricing{"pr-1": testSheet()})

	newLC := newParityLC()
	obs := (&StreamUsageFactory{pricer: pricer}).NewObserver(newLC)
	for _, f := range frames {
		obs.Observe(f)
	}
	got, err := obs.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	newEv := got.(*Event)

	oldEv := oldStreamUsageResult(newParityLC(), pricer, frames)

	if !reflect.DeepEqual(newEv.Tokens, oldEv.Tokens) {
		t.Fatalf("tokens: incremental %+v != accumulate %+v", newEv.Tokens, oldEv.Tokens)
	}
	if newEv.FinishReason != oldEv.FinishReason {
		t.Fatalf("finish: %q != %q", newEv.FinishReason, oldEv.FinishReason)
	}
	if !reflect.DeepEqual(newEv.CostNanos, oldEv.CostNanos) || !reflect.DeepEqual(newEv.CostBreakdown, oldEv.CostBreakdown) {
		t.Fatalf("cost mismatch: %v/%+v vs %v/%+v", newEv.CostNanos, newEv.CostBreakdown, oldEv.CostNanos, oldEv.CostBreakdown)
	}
	if newEv.Tokens["input"] != 1000 || newEv.Tokens["output"] != 200 || newEv.Tokens["cache_read"] != 50 {
		t.Fatalf("tokens wrong: %+v", newEv.Tokens)
	}
	if newEv.FinishReason != "stop" {
		t.Fatalf("finish wrong: %q", newEv.FinishReason)
	}
	if newEv.CostNanos == nil {
		t.Fatal("priced stream: CostNanos nil")
	}
}

func newParityLC() *lifecycle.Context {
	lc := lifecycle.NewContext("req-parity", "pipeline", time.Now())
	lc.Streamed = true
	lc.PricingID, lc.PricingName = "pr-1", "anthropic-opus"
	// Identity translator: the wire IS canonical SSE, so both ExtractSummary
	// and StreamSummarizer harvest the generation.completed event directly.
	lc.Translator = stubTranslator{}
	return lc
}
