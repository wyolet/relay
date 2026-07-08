package pipeline

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/pkg/lifecycle"
)

// Post-flight must run Finalize (usage/payload hooks need the response body)
// before the rate-limit commits, so the body reference can be dropped ahead of
// the slow Valkey commit RTTs. This pins both halves of the invariant: the
// hook still sees the body, and no commit has landed by the time Finalize runs.
func TestRunPostFlight_FinalizeBeforeCommits_HookSeesBody(t *testing.T) {
	t.Parallel()

	h := newReservationHarness()

	type observed struct {
		commitsAtFinalize int
		body              string
	}
	got := make(chan observed, 1)
	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{HookName: "order", Fn: func(_ *lifecycle.Context, ev *lifecycle.PostFlightEvent) (any, error) {
		got <- observed{
			commitsAtFinalize: len(h.store.snapshotCommits()),
			body:              string(ev.ResponseBody),
		}
		return nil, nil
	}})
	h.pipeline.Lifecycle = reg

	res, err := h.pipeline.Run(context.Background(), &Request{
		Adapter:   reservationTestAdapter{},
		Keys:      []*hostkey.HostKey{h.key},
		Policy:    h.requestPolicy,
		Lifecycle: lifecycle.NewContext("req-order", "pipeline", time.Now()),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	if err := res.Body.Close(); err != nil {
		t.Fatalf("Body.Close: %v", err)
	}

	var obs observed
	select {
	case obs = <-got:
	case <-time.After(time.Second):
		t.Fatal("post-flight hook did not run")
	}
	// Drain the two commits so they can't outlive the test goroutine.
	waitForCommits(t, h.store, 2)

	if obs.commitsAtFinalize != 0 {
		t.Fatalf("commits observed during Finalize = %d, want 0 — Finalize must precede the rate-limit commits", obs.commitsAtFinalize)
	}
	if obs.body != "ok" {
		t.Fatalf("Finalize saw ResponseBody %q, want %q — hooks must still see the body before it is dropped", obs.body, "ok")
	}
}
