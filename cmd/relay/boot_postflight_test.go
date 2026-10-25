package main

import (
	"testing"
	"time"

	"github.com/wyolet/relay/app/tokencount"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/internal/config"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	"github.com/wyolet/relay/pkg/lifecycle"
	sdkusage "github.com/wyolet/relay/sdk/usage"
)

// sharedAndPostFlight records the calls of the shared and the post-flight store separately, over one keyspace.
func sharedAndPostFlight(t *testing.T) (shared, post *kvtest.Recording) {
	t.Helper()
	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	return kvtest.NewRecording(mem), kvtest.NewRecording(mem)
}

func TestOpenKVInMemoryIsOneStore(t *testing.T) {
	shared, post := openKV(t.Context(), &config.Config{StateBackend: "memory"})
	t.Cleanup(func() { _ = shared.Close() })
	if shared != post {
		t.Fatal("in memory the shared and post-flight stores must be one store")
	}
}

func TestPostFlightHostHealthWritesOnPostFlightStore(t *testing.T) {
	shared, post := sharedAndPostFlight(t)
	newPostFlightWriters(post).hostHealth.Reachable(t.Context(), "host-1")
	if shared.RoundTrips() != 0 || post.RoundTrips() != 1 {
		t.Fatalf("round trips: shared %d, post-flight %d; want 0, 1", shared.RoundTrips(), post.RoundTrips())
	}
}

// The ratio is written by the post-flight collector and read for count-tokens on the shared store.
func TestTokenCalibratorSplitsWritesFromReads(t *testing.T) {
	shared, post := sharedAndPostFlight(t)
	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{HookName: usagelog.Namespace, Fn: func(*lifecycle.Context, *lifecycle.PostFlightEvent) (any, error) {
		return &usagelog.Event{Tokens: sdkusage.Tokens{"input": 1000}}, nil
	}})
	cal := buildTokenCalibrator(shared, post, reg)

	lc := lifecycle.NewContext("req-1", "pipeline", time.Now())
	lc.RequestBytes, lc.ModelID = 4000, "model-x"
	lc.Metadata[tokencount.MetadataKeySession] = "sess-x"
	reg.Finalize(t.Context(), lc, &lifecycle.PostFlightEvent{Status: 200})
	if shared.RoundTrips() != 0 || post.RoundTrips() != 2 {
		t.Fatalf("after post-flight: shared %d, post-flight %d round trips; want 0, 2 (session and model ratio)", shared.RoundTrips(), post.RoundTrips())
	}

	if r, ok := cal.Ratio(t.Context(), "sess-x", "model-x"); !ok || r != 0.25 {
		t.Fatalf("ratio = %v (ok=%v), want 0.25", r, ok)
	}
	if shared.RoundTrips() != 1 || post.RoundTrips() != 2 {
		t.Fatalf("after the read: shared %d, post-flight %d round trips; want 1, 2", shared.RoundTrips(), post.RoundTrips())
	}
}
