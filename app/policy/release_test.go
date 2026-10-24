package policy

import (
	"context"
	"testing"
	"time"

	"github.com/wyolet/relay/app/keypool"
	appratelimit "github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// oneSlotRule lets a key carry one request at a time, so a held slot shows as a refused acquire.
func oneSlotRule() appratelimit.Rule {
	return appratelimit.Rule{
		Meter: appratelimit.MeterConcurrency, Amount: 1,
		Window: appratelimit.Window(time.Minute), Strategy: appratelimit.StrategySlidingWindow,
	}
}

// newReleaseFixture admits a request on store holding the first key's only slot; the service's post-flight writers run on post, another client of the same store.
func newReleaseFixture(t *testing.T, store kvtest.ScriptStore) (admitFixture, *kvtest.Recording, Admission) {
	t.Helper()
	f := newAdmitFixture(t, store, keypool.KeySelectionPrioritized, 10, oneSlotRule())
	post := kvtest.NewRecording(store)
	f.svc.WithPostFlight(keypool.New(post, discardLogger(), nil, nil), pkgratelimit.New(post, discardLogger(), nil))
	adm, err := f.svc.Admit(context.Background(), f.in, f.acq)
	if err != nil || adm.AcqErr != nil || adm.Acq == nil || adm.Acq.Key != f.keys[0] || adm.Acq.Reservation == nil {
		t.Fatalf("Admit = %+v, %v; want the first key with its slot", adm, err)
	}
	return f, post, adm
}

// requireSlotFree acquires the first key again, which succeeds only once its slot was refunded.
func requireSlotFree(t *testing.T, f admitFixture) {
	t.Helper()
	acq, err := f.svc.Acquire(context.Background(), f.acq)
	if err != nil || acq.Key != f.keys[0] {
		t.Fatalf("Acquire after the refund = %+v, %v; want the first key's slot back", acq, err)
	}
}

// runReleaseSuite pins the failure writes on one backend: they go to the post-flight store, a failed attempt's refund and key failure in one round trip, a failed request's two commits in one.
func runReleaseSuite(t *testing.T, newStore func(t *testing.T) kvtest.ScriptStore) {
	t.Run("release_refunds_and_records_in_one_batch", func(t *testing.T) {
		f, post, adm := newReleaseFixture(t, newStore(t))
		ctx := context.Background()
		before := f.store.RoundTrips()
		f.svc.Release(ctx, adm.Acq, keypool.FailureServerError, 0)
		if trips := f.store.RoundTrips() - before; trips != 0 {
			t.Fatalf("shared store round trips = %d during Release, want none", trips)
		}
		if b := post.Batches(); len(b) != 1 || b[0] != 2 || post.RoundTrips() != 1 {
			t.Fatalf("post-flight batches = %v in %d round trips, want one of 2 (the refund and the failure write)", b, post.RoundTrips())
		}
		if n, r := len(post.Named("limit.commit")), len(post.Named("keypool.record_failure")); n != 1 || r != 1 {
			t.Fatalf("post-flight scripts = %d commits, %d failure writes; want 1, 1", n, r)
		}
		rec, found := keypool.New(post, discardLogger(), nil, nil).ReadCircuit(ctx, f.keys[0].KeyHash)
		if !found || rec.Failures != 1 {
			t.Fatalf("record after Release = %+v (found %v), want one counted failure", rec, found)
		}
		requireSlotFree(t, f)
	})

	t.Run("release_with_no_key_write_refunds_only", func(t *testing.T) {
		f, post, adm := newReleaseFixture(t, newStore(t))
		ctx := context.Background()
		f.svc.Release(ctx, adm.Acq, keypool.FailureRateLimitShort, 0)
		if b := post.Batches(); len(b) != 1 || b[0] != 1 {
			t.Fatalf("post-flight batches = %v, want one of 1 (the refund)", b)
		}
		if n := len(post.Named("keypool.record_failure")); n != 0 {
			t.Fatalf("failure writes = %d, want none for a short rate limit", n)
		}
		requireSlotFree(t, f)
	})

	t.Run("settle_failed_commits_both_in_one_batch", func(t *testing.T) {
		f, post, adm := newReleaseFixture(t, newStore(t))
		ctx := context.Background()
		before := f.store.RoundTrips()
		if err := f.svc.SettleFailed(ctx, adm.Inbound, adm.Acq, pkgratelimit.Observations{Cancelled: true}); err != nil {
			t.Fatalf("SettleFailed: %v", err)
		}
		if trips := f.store.RoundTrips() - before; trips != 0 {
			t.Fatalf("shared store round trips = %d during SettleFailed, want none", trips)
		}
		if b := post.Batches(); len(b) != 1 || b[0] != 2 || post.RoundTrips() != 1 {
			t.Fatalf("post-flight batches = %v in %d round trips, want one of 2 (inbound and upstream)", b, post.RoundTrips())
		}
		if n := len(post.Named("keypool.record_failure")) + len(post.Named("keypool.record_success")); n != 0 {
			t.Fatalf("key writes = %d, want none", n)
		}
		requireSlotFree(t, f)
	})

	t.Run("nil_shapes_write_nothing", func(t *testing.T) {
		f, post, _ := newReleaseFixture(t, newStore(t))
		ctx := context.Background()
		f.svc.Release(ctx, nil, keypool.FailureAuth, 0)
		f.svc.Release(ctx, &Acquisition{}, keypool.FailureAuth, 0)
		if err := f.svc.SettleFailed(ctx, nil, nil, pkgratelimit.Observations{}); err != nil {
			t.Fatalf("SettleFailed(nil, nil): %v", err)
		}
		if trips := post.RoundTrips(); trips != 0 {
			t.Fatalf("post-flight round trips = %d, want none", trips)
		}
	})
}

func TestReleaseMem(t *testing.T) {
	runReleaseSuite(t, func(t *testing.T) kvtest.ScriptStore { return admitMem(t) })
}

// With no store wired the failure writes are no-ops, not panics.
func TestReleaseWithoutStores(t *testing.T) {
	svc := NewService(reserveSnap{}, nil, nil)
	ctx := context.Background()
	svc.Release(ctx, &Acquisition{Reservation: &pkgratelimit.Reservation{}}, keypool.FailureAuth, 0)
	if err := svc.SettleFailed(ctx, &pkgratelimit.Reservation{}, nil, pkgratelimit.Observations{Cancelled: true}); err != nil {
		t.Fatalf("SettleFailed: %v", err)
	}
}
