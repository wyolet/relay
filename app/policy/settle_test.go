package policy

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// runSettleSuite pins Settle on one backend: the commits and, only when the picked key's record needs it, the success write go out in one round trip.
func runSettleSuite(t *testing.T, newStore func(t *testing.T) kvtest.ScriptStore) {
	obs := pkgratelimit.Observations{}

	t.Run("clean_key_commits_only", func(t *testing.T) {
		f := newAdmitFixture(t, newStore(t), keypool.KeySelectionPrioritized, 10, concurrencyRule())
		ctx := context.Background()
		adm, err := f.svc.Admit(ctx, f.in, f.acq)
		if err != nil || adm.AcqErr != nil || adm.Acq.Reservation == nil {
			t.Fatalf("Admit = %+v, %v; want an inbound and an upstream reservation", adm, err)
		}
		before, beforeBatches := f.store.RoundTrips(), len(f.store.Batches())
		if err := f.svc.Settle(ctx, adm.Inbound, adm.Acq, obs); err != nil {
			t.Fatalf("Settle: %v", err)
		}
		batches, size := batchStats(f.store)
		if trips := f.store.RoundTrips() - before; trips != 1 || batches-beforeBatches != 1 || size != 2 {
			t.Fatalf("round trips = %d, batches +%d of %d; want 1 batch of the two commits", trips, batches-beforeBatches, size)
		}
		if n := len(f.store.Named("keypool.record_success")); n != 0 {
			t.Fatalf("success writes = %d, want none for a key with no record", n)
		}
	})

	t.Run("nothing_to_settle", func(t *testing.T) {
		f := newAdmitFixture(t, newStore(t), keypool.KeySelectionPrioritized, 10)
		ctx := context.Background()
		if err := f.svc.Settle(ctx, nil, nil, obs); err != nil {
			t.Fatal(err)
		}
		if err := f.svc.Settle(ctx, nil, &Acquisition{Key: f.keys[0]}, obs); err != nil {
			t.Fatal(err)
		}
		if trips := f.store.RoundTrips(); trips != 0 {
			t.Fatalf("round trips = %d, want none", trips)
		}
	})

	t.Run("half_open_probe_writes_success", func(t *testing.T) {
		f := newAdmitFixture(t, newStore(t), keypool.KeySelectionPrioritized, 10, concurrencyRule())
		ctx := context.Background()
		sel := keypool.New(f.store, discardLogger(), nil, nil)
		// Open with a cooldown that has already run out: the next pick probes.
		sel.RecordFailure(ctx, f.keys[0].KeyHash, keypool.FailureRateLimitLong, 0)
		adm, err := f.svc.Admit(ctx, f.in, f.acq)
		if err != nil || adm.Acq == nil || adm.Acq.Key != f.keys[0] {
			t.Fatalf("Admit = %+v, %v; want the probe on the first key", adm, err)
		}
		beforeBatches := len(f.store.Batches())
		if err := f.svc.Settle(ctx, adm.Inbound, adm.Acq, obs); err != nil {
			t.Fatalf("Settle: %v", err)
		}
		batches, size := batchStats(f.store)
		if batches-beforeBatches != 1 || size != 3 {
			t.Fatalf("batches +%d of %d, want 1 of 3 (two commits and the success write)", batches-beforeBatches, size)
		}
		rec, found := sel.ReadCircuit(ctx, f.keys[0].KeyHash)
		if !found || rec.State != keypool.CircuitClosed {
			t.Fatalf("record after the probe's success = %+v (found %v), want closed", rec, found)
		}

		// The record is now clean: the next success writes nothing.
		adm, _ = f.svc.Admit(ctx, f.in, f.acq)
		beforeBatches = len(f.store.Batches())
		if err := f.svc.Settle(ctx, adm.Inbound, adm.Acq, obs); err != nil {
			t.Fatalf("Settle: %v", err)
		}
		if batches, size := batchStats(f.store); batches-beforeBatches != 1 || size != 2 {
			t.Fatalf("batches +%d of %d after recovery, want 1 of 2", batches-beforeBatches, size)
		}
	})

	t.Run("post_flight_store_settles", func(t *testing.T) {
		store := newStore(t)
		f := newAdmitFixture(t, store, keypool.KeySelectionPrioritized, 10, concurrencyRule())
		post := kvtest.NewRecording(store)
		f.svc.WithPostFlight(keypool.New(post, discardLogger(), nil, nil), pkgratelimit.New(post, discardLogger(), nil))
		ctx := context.Background()
		// A half-open probe, so the settle carries the success write too.
		keypool.New(f.store, discardLogger(), nil, nil).RecordFailure(ctx, f.keys[0].KeyHash, keypool.FailureRateLimitLong, 0)
		adm, err := f.svc.Admit(ctx, f.in, f.acq)
		if err != nil || adm.Acq == nil || adm.Acq.Reservation == nil {
			t.Fatalf("Admit = %+v, %v; want an inbound and an upstream reservation", adm, err)
		}
		before := f.store.RoundTrips()
		if err := f.svc.Settle(ctx, adm.Inbound, adm.Acq, obs); err != nil {
			t.Fatalf("Settle: %v", err)
		}
		if trips := f.store.RoundTrips() - before; trips != 0 {
			t.Fatalf("shared store round trips = %d during Settle, want none", trips)
		}
		if b := post.Batches(); len(b) != 1 || b[0] != 3 {
			t.Fatalf("post-flight batches = %v, want one of 3 (two commits and the success write)", b)
		}
		if n, s := len(post.Named("limit.commit")), len(post.Named("keypool.record_success")); n != 2 || s != 1 {
			t.Fatalf("post-flight scripts = %d commits, %d success writes; want 2, 1", n, s)
		}
	})
}

func TestSettleMem(t *testing.T) {
	runSettleSuite(t, func(t *testing.T) kvtest.ScriptStore { return admitMem(t) })
}
