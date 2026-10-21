package policy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/meta"
	appratelimit "github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// admitSnap serves the caller and tier policies and their rate limits by id.
type admitSnap struct {
	pols map[string]*Policy
	rls  map[string]*appratelimit.RateLimit
}

func (s admitSnap) Policy(_ context.Context, id string) (*Policy, bool) {
	p, ok := s.pols[id]
	return p, ok
}

func (s admitSnap) RateLimit(_ context.Context, id string) (*appratelimit.RateLimit, bool) {
	rl, ok := s.rls[id]
	return rl, ok
}

type admitFixture struct {
	svc   *Service
	store *kvtest.Recording
	in    InboundInput
	acq   AcquireInput
	keys  []*hostkey.HostKey
}

// newAdmitFixture: a caller policy metering requests (limit inboundAmount)
// over two keys whose tier policy meters tierRules (none: unmetered), on
// store.
func newAdmitFixture(t *testing.T, store kvtest.ScriptStore, algo keypool.KeySelection, inboundAmount int64, tierRules ...appratelimit.Rule) admitFixture {
	t.Helper()
	rls := map[string]*appratelimit.RateLimit{}
	tier := &Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "upstream-tier", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	if len(tierRules) > 0 {
		tierRL := testRateLimit("tier-rl", tierRules...)
		tier.Spec.RateLimitID = tierRL.Meta.ID
		rls[tierRL.Meta.ID] = tierRL
	}
	var keys []*hostkey.HostKey
	for _, name := range []string{"key-a", "key-b"} {
		k := &hostkey.HostKey{Meta: meta.Metadata{ID: meta.NewID(), Name: name}, Spec: hostkey.Spec{PolicyID: tier.Meta.ID}}
		k.KeyHash = "hash-" + k.Meta.ID
		keys = append(keys, k)
	}
	rule := requestsRule()
	rule.Amount = inboundAmount
	rl := testRateLimit("caller-rl", rule)
	rls[rl.Meta.ID] = rl
	caller := fix("caller")
	caller.Meta.ID = meta.NewID()
	caller.Spec.RateLimitID = rl.Meta.ID
	caller.Spec.KeySelection = algo

	rec := kvtest.NewRecording(store)
	svc := NewService(
		admitSnap{pols: map[string]*Policy{tier.Meta.ID: tier, caller.Meta.ID: caller}, rls: rls},
		keypool.New(rec, discardLogger(), nil, nil),
		pkgratelimit.New(rec, discardLogger(), nil),
	)
	return admitFixture{
		svc: svc, store: rec, keys: keys,
		in:  InboundInput{Policy: caller, TeamID: "team-1"},
		acq: AcquireInput{Policy: caller, Keys: keys, Provider: "acme"},
	}
}

// runAdmitSuite pins Admit on one backend: the inbound reserve and the first
// pick cost one round trip together, and the outcome is what ReserveInbound
// followed by Acquire would have returned.
func runAdmitSuite(t *testing.T, newStore func(t *testing.T) kvtest.ScriptStore) {
	t.Run("one_round_trip", func(t *testing.T) {
		f := newAdmitFixture(t, newStore(t), keypool.KeySelectionRoundRobin, 10)
		ctx := context.Background()
		adm, err := f.svc.Admit(ctx, f.in, f.acq)
		if err != nil {
			t.Fatalf("Admit: %v", err)
		}
		if adm.Inbound == nil || adm.AcqErr != nil || adm.Acq == nil || adm.Acq.Key != f.keys[0] {
			t.Fatalf("admission = %+v, want an inbound reservation and the first key", adm)
		}
		want := 1 + len(f.keys) + 2 // reserve, breaker reads, INCR, EXPIRE NX
		if trips, batches := f.store.RoundTrips(), f.store.Batches(); trips != 1 || len(batches) != 1 || batches[0] != want {
			t.Fatalf("round trips = %d, batches = %v; want 1 of %d commands", trips, batches, want)
		}
		adm, _ = f.svc.Admit(ctx, f.in, f.acq)
		if adm.Acq == nil || adm.Acq.Key != f.keys[1] {
			t.Fatalf("second admission picked %+v, want the rotation to move on", adm.Acq)
		}
	})

	t.Run("prioritized_reads_one_record", func(t *testing.T) {
		f := newAdmitFixture(t, newStore(t), keypool.KeySelectionPrioritized, 10)
		if _, err := f.svc.Admit(context.Background(), f.in, f.acq); err != nil {
			t.Fatalf("Admit: %v", err)
		}
		if batches := f.store.Batches(); len(batches) != 1 || batches[0] != 2 {
			t.Fatalf("batches = %v, want one of 2 (reserve, first record)", batches)
		}
	})

	t.Run("refused_reserve_skips_the_pick", func(t *testing.T) {
		f := newAdmitFixture(t, newStore(t), keypool.KeySelectionRoundRobin, 1)
		ctx := context.Background()
		if _, err := f.svc.Admit(ctx, f.in, f.acq); err != nil {
			t.Fatalf("first Admit: %v", err)
		}
		before := f.store.RoundTrips()
		adm, err := f.svc.Admit(ctx, f.in, f.acq)
		if !errors.Is(err, pkgratelimit.ErrExceeded) {
			t.Fatalf("err = %v, want ErrExceeded", err)
		}
		if adm != (Admission{}) {
			t.Fatalf("admission = %+v, want none on a refused reserve", adm)
		}
		if trips := f.store.RoundTrips() - before; trips != 1 {
			t.Fatalf("round trips = %d for a refused admission, want 1", trips)
		}
	})

	t.Run("failed_pick_keeps_the_reservation", func(t *testing.T) {
		f := newAdmitFixture(t, newStore(t), keypool.KeySelectionRoundRobin, 10)
		f.acq.Excluded = f.keys
		adm, err := f.svc.Admit(context.Background(), f.in, f.acq)
		if err != nil {
			t.Fatalf("Admit: %v", err)
		}
		if adm.Inbound == nil {
			t.Fatal("no inbound reservation: the caller could not settle it")
		}
		if !errors.Is(adm.AcqErr, keypool.ErrNoHealthyKeys) || adm.Acq != nil {
			t.Fatalf("acquisition = %+v, %v; want ErrNoHealthyKeys", adm.Acq, adm.AcqErr)
		}
	})
}

func TestAdmitMem(t *testing.T) {
	runAdmitSuite(t, func(t *testing.T) kvtest.ScriptStore { return admitMem(t) })
}

func admitMem(t *testing.T) *kv.Mem {
	t.Helper()
	m := kv.NewMem()
	pkgratelimit.RegisterScripts(m)
	keypool.RegisterScripts(m)
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// faultyMem fails the breaker reads and, when failReserve is set, the
// reserve script.
type faultyMem struct {
	*kv.Mem
	failReserve bool
}

var errInjected = errors.New("injected kv failure")

func (f *faultyMem) Get(ctx context.Context, key string) ([]byte, error) {
	if strings.HasPrefix(key, "secret_health:") {
		return nil, errInjected
	}
	return f.Mem.Get(ctx, key)
}

func (f *faultyMem) RunScript(ctx context.Context, name, script string, keys []string, args ...any) ([]byte, error) {
	if f.failReserve && name == "limit.reserve" {
		return nil, errInjected
	}
	return f.Mem.RunScript(ctx, name, script, keys, args...)
}

// A breaker read that fails counts as a closed record, as a single read does;
// a reserve that fails fails the admission.
func TestAdmit_PartialFailure(t *testing.T) {
	ctx := context.Background()
	t.Run("breaker_read", func(t *testing.T) {
		f := newAdmitFixture(t, &faultyMem{Mem: admitMem(t)}, keypool.KeySelectionPrioritized, 10)
		adm, err := f.svc.Admit(ctx, f.in, f.acq)
		if err != nil || adm.AcqErr != nil || adm.Acq == nil || adm.Acq.Key != f.keys[0] {
			t.Fatalf("admission = %+v, %v; want the first key, read as closed", adm, err)
		}
	})
	t.Run("reserve", func(t *testing.T) {
		f := newAdmitFixture(t, &faultyMem{Mem: admitMem(t), failReserve: true}, keypool.KeySelectionPrioritized, 10)
		adm, err := f.svc.Admit(ctx, f.in, f.acq)
		if !errors.Is(err, errInjected) || adm != (Admission{}) {
			t.Fatalf("admission = %+v, %v; want the reserve's failure and nothing else", adm, err)
		}
	})
}
