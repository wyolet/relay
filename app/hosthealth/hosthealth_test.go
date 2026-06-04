package hosthealth_test

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hosthealth"
	"github.com/wyolet/relay/pkg/kv"
)

func TestReachable_ThenUnreachable_TracksState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := hosthealth.New(kv.NewMem(), nil)

	if _, found := r.Read(ctx, "h1"); found {
		t.Fatal("expected no record before any observation")
	}

	r.Reachable(ctx, "h1")
	st, found := r.Read(ctx, "h1")
	if !found || st.Health != host.HealthHealthy {
		t.Fatalf("after Reachable: found=%v health=%q, want true/healthy", found, st.Health)
	}
	if st.LastSuccess.IsZero() {
		t.Error("LastSuccess should be set when healthy")
	}

	r.Unreachable(ctx, "h1", "dial tcp: connection refused")
	st, _ = r.Read(ctx, "h1")
	if st.Health != host.HealthUnreachable {
		t.Errorf("health = %q, want unreachable", st.Health)
	}
	if st.ConsecutiveFailures != 1 {
		t.Errorf("ConsecutiveFailures = %d, want 1", st.ConsecutiveFailures)
	}
	if st.LastSuccess.IsZero() {
		t.Error("LastSuccess should be preserved across an unreachable transition")
	}

	r.Unreachable(ctx, "h1", "dial tcp: connection refused")
	st, _ = r.Read(ctx, "h1")
	if st.ConsecutiveFailures != 2 {
		t.Errorf("ConsecutiveFailures = %d, want 2 (accumulates)", st.ConsecutiveFailures)
	}

	// Recovery clears the failure counter.
	r.Reachable(ctx, "h1")
	st, _ = r.Read(ctx, "h1")
	if st.Health != host.HealthHealthy || st.ConsecutiveFailures != 0 {
		t.Errorf("after recovery: health=%q fails=%d, want healthy/0", st.Health, st.ConsecutiveFailures)
	}
}

func TestNilRecorder_Safe(t *testing.T) {
	t.Parallel()
	var r *hosthealth.Recorder
	r.Reachable(context.Background(), "h1")            // must not panic
	r.Unreachable(context.Background(), "h1", "boom")  // must not panic
	if _, found := r.Read(context.Background(), "h1"); found {
		t.Error("nil recorder must report no record")
	}
}
