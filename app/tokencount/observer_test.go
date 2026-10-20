package tokencount

import (
	"testing"
	"time"

	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/lifecycle"
	sdkusage "github.com/wyolet/relay/sdk/usage"
)

// finalized runs one request through a lifecycle Registry carrying a usage producer, exactly as a runner's post-flight does.
func finalized(t *testing.T, o *Observer, lc *lifecycle.Context, ev *usagelog.Event) {
	t.Helper()
	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{HookName: usagelog.Namespace, Fn: func(*lifecycle.Context, *lifecycle.PostFlightEvent) (any, error) {
		return ev, nil
	}})
	reg.RegisterCollector(o)
	reg.Finalize(t.Context(), lc, &lifecycle.PostFlightEvent{Status: 200})
}

func newLC(requestBytes int, modelID, session string) *lifecycle.Context {
	lc := lifecycle.NewContext("req-1", "pipeline", time.Now())
	lc.RequestBytes = requestBytes
	lc.ModelID = modelID
	if session != "" {
		lc.Metadata[MetadataKeySession] = session
	}
	return lc
}

func TestObserver_RecordsTheRatio(t *testing.T) {
	s := kv.NewMem()
	t.Cleanup(func() { _ = s.Close() })
	cal := NewCalibrator(s)

	finalized(t, NewObserver(cal), newLC(4000, "model-x", "sess-x"),
		&usagelog.Event{Tokens: sdkusage.Tokens{"input": 1000, "output": 20}})

	got, ok := cal.Ratio(t.Context(), "sess-x", "model-x")
	if !ok || got != 0.25 {
		t.Fatalf("ratio = %v (ok=%v), want 0.25", got, ok)
	}
}

// On a cache hit "input" is only the uncached remainder; the body still carries the cached prefix, so the ratio must count cache reads and writes too.
func TestObserver_CountsCachedPromptTokens(t *testing.T) {
	s := kv.NewMem()
	t.Cleanup(func() { _ = s.Close() })
	cal := NewCalibrator(s)

	finalized(t, NewObserver(cal), newLC(4000, "model-x", "sess-x"),
		&usagelog.Event{Tokens: sdkusage.Tokens{"input": 100, "cache_read": 600, "cache_creation": 300, "output": 20}})

	got, ok := cal.Ratio(t.Context(), "sess-x", "model-x")
	if !ok || got != 0.25 {
		t.Fatalf("ratio = %v (ok=%v), want 0.25 from 1000 prompt tokens", got, ok)
	}
}

// A request with no body, or which never resolved to a model, or which reported no input tokens, measures nothing.
func TestObserver_SkipsUnmeasurableRequests(t *testing.T) {
	cases := []struct {
		name string
		lc   *lifecycle.Context
		ev   *usagelog.Event
	}{
		{"no model", newLC(4000, "", "s"), &usagelog.Event{Tokens: sdkusage.Tokens{"input": 1000}}},
		{"no body", newLC(0, "m", "s"), &usagelog.Event{Tokens: sdkusage.Tokens{"input": 1000}}},
		{"no input tokens", newLC(4000, "m", "s"), &usagelog.Event{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := kv.NewMem()
			t.Cleanup(func() { _ = s.Close() })
			cal := NewCalibrator(s)
			finalized(t, NewObserver(cal), tc.lc, tc.ev)
			if _, ok := cal.Ratio(t.Context(), "s", "m"); ok {
				t.Fatal("recorded a ratio from a request that measures nothing")
			}
		})
	}
}
