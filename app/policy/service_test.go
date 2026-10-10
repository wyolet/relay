package policy

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	appratelimit "github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// acquireFixture wires a Service over a real in-memory limiter and selector
// with one host key whose tier policy carries rules.
type acquireFixture struct {
	svc   *Service
	store *kvtest.Recording
	key   *hostkey.HostKey
	tier  *Policy
	in    AcquireInput
}

func newAcquireFixture(t *testing.T, tierRules ...appratelimit.Rule) acquireFixture {
	t.Helper()
	hostID := meta.NewID()
	h := &host.Host{Meta: meta.Metadata{ID: hostID, Name: "upstream", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	m := &model.Model{Meta: meta.Metadata{ID: meta.NewID(), Name: "m1", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: meta.NewID()}}}
	tier := &Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "upstream-tier", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}}}

	var rl *appratelimit.RateLimit
	if len(tierRules) > 0 {
		rl = testRateLimit("tier-rl", tierRules...)
		tier.Spec.RateLimitID = rl.Meta.ID
	}
	k := &hostkey.HostKey{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "upstream-key", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}},
		Spec: hostkey.Spec{HostID: hostID, PolicyID: tier.Meta.ID},
	}
	k.KeyHash = "hash-" + k.Meta.ID

	store := recordingMem(t)
	caller := fix("caller")
	caller.Meta.ID = meta.NewID()
	svc := NewService(
		reserveSnap{pol: tier, rl: rl},
		keypool.New(store, discardLogger(), nil, nil),
		pkgratelimit.New(store, discardLogger(), nil),
	)
	return acquireFixture{
		svc: svc, store: store, key: k, tier: tier,
		in: AcquireInput{Policy: caller, Keys: []*hostkey.HostKey{k}, Model: m, Host: h, Provider: "acme"},
	}
}

// Acquire meters the chosen key against its own host tier, in a bucket scoped
// to the key rather than to the caller's policy: two customers sharing one
// upstream credential share its upstream cap.
func TestAcquire_TierRateLimit(t *testing.T) {
	f := newAcquireFixture(t, requestsRule())

	acq, err := f.svc.Acquire(context.Background(), f.in)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if acq.Key != f.key {
		t.Fatalf("key = %+v, want the only candidate", acq.Key)
	}
	if acq.Reservation == nil {
		t.Fatal("no reservation: the tier's rules were not applied to the upstream bucket")
	}
	if got := acq.KeyHash(); got != f.key.KeyHash {
		t.Errorf("KeyHash = %q, want %q", got, f.key.KeyHash)
	}
	reserves := f.store.Named("limit.reserve")
	if len(reserves) != 1 {
		t.Fatalf("reserve scripts = %d, want exactly 1", len(reserves))
	}
	wantScope := "limit:{hostkey:" + f.key.Meta.ID + "}:"
	for _, key := range reserves[0].Keys {
		if !strings.HasPrefix(key, wantScope) {
			t.Errorf("key %q is not under the key's own hash tag %q", key, wantScope)
		}
		if !strings.Contains(key, "policy:upstream-tier:rl:tier-rl") {
			t.Errorf("key %q does not name the tier and its rate limit", key)
		}
	}

	// Commit returns the reservation with the observed usage; a nil
	// acquisition or one that never reserved is a no-op.
	if err := f.svc.Commit(context.Background(), acq, pkgratelimit.Observations{}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := f.svc.Commit(context.Background(), nil, pkgratelimit.Observations{}); err != nil {
		t.Fatalf("Commit(nil): %v", err)
	}
	if err := f.svc.Commit(context.Background(), &Acquisition{Key: f.key}, pkgratelimit.Observations{}); err != nil {
		t.Fatalf("Commit without a reservation: %v", err)
	}
}

// A tier whose budget is spent takes the key out of play for this request:
// the caller is told to exclude it and retry, and the key is marked so the
// selector stops offering it.
func TestAcquire_SaturatedTierExcludesTheKey(t *testing.T) {
	oneRequest := requestsRule()
	oneRequest.Amount = 1
	f := newAcquireFixture(t, oneRequest)
	ctx := context.Background()

	first, err := f.svc.Acquire(ctx, f.in)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if err := f.svc.Commit(ctx, first, pkgratelimit.Observations{}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	acq, err := f.svc.Acquire(ctx, f.in)
	if !errors.Is(err, ErrSaturated) {
		t.Fatalf("second Acquire err = %v, want ErrSaturated", err)
	}
	if acq == nil || acq.Key != f.key {
		t.Fatal("ErrSaturated must still name the key, so the caller can exclude it")
	}
	if acq.Reservation != nil {
		t.Error("a saturated acquisition carries a reservation nothing will commit")
	}

	// Excluding it leaves no candidate at all.
	excluded := f.in
	excluded.Excluded = []*hostkey.HostKey{f.key}
	if _, err := f.svc.Acquire(ctx, excluded); err == nil {
		t.Fatal("Acquire with every candidate excluded should fail")
	}
}

// A tier with nothing to meter costs no kv round trip, and neither does a
// key whose tier has left the snapshot — an unmetered request beats a
// failed one, so the miss is tolerated rather than fatal.
func TestAcquire_NoTierRulesMakesNoReservation(t *testing.T) {
	for _, tc := range []struct {
		name string
		snap SnapshotReader
	}{
		{name: "the tier caps nothing", snap: nil},
		{name: "the tier is gone", snap: reserveSnap{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAcquireFixture(t)
			if tc.snap != nil {
				f.svc.snap = tc.snap
			}
			acq, err := f.svc.Acquire(context.Background(), f.in)
			if err != nil {
				t.Fatalf("Acquire: %v", err)
			}
			if acq.Reservation != nil {
				t.Error("a reservation was made with no rules to meter by")
			}
			if got := len(f.store.Named("limit.reserve")); got != 0 {
				t.Errorf("reserve scripts = %d, want 0", got)
			}
		})
	}
}

// Acquire without a selector is a wiring error, not a request-time failure
// mode, so it is reported rather than silently serving keyless.
func TestAcquire_WithoutASelectorIsAnError(t *testing.T) {
	svc := NewService(reserveSnap{}, nil, nil)
	if _, err := svc.Acquire(context.Background(), AcquireInput{}); err == nil {
		t.Fatal("Acquire without a selector should fail")
	}
}

// Release rolls the upstream reservation back with zero usage and records the
// failure against the key, so a failed request neither spends budget nor
// leaves the key looking healthy.
func TestRelease_RefundsAndRecordsTheFailure(t *testing.T) {
	f := newAcquireFixture(t, requestsRule())
	ctx := context.Background()
	acq, err := f.svc.Acquire(ctx, f.in)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	f.svc.Release(ctx, acq, keypool.FailureAuth, 0)
	if got := len(f.store.Named("limit.commit")); got != 1 {
		t.Fatalf("commit scripts = %d, want the refund", got)
	}
	// The breaker for this key is now open, so it is no longer selectable.
	if _, err := f.svc.Acquire(ctx, f.in); err == nil {
		t.Fatal("the key is still selectable after an auth failure was recorded")
	}

	// Every nil shape is a no-op rather than a panic on the failure path.
	f.svc.Release(ctx, nil, keypool.FailureAuth, 0)
	f.svc.Release(ctx, &Acquisition{}, keypool.FailureAuth, 0)
	f.svc.RecordSuccess(ctx, nil)
	f.svc.RecordSuccess(ctx, &Acquisition{})
	if got := (*Acquisition)(nil).KeyHash(); got != "" {
		t.Errorf("nil acquisition KeyHash = %q, want empty", got)
	}
}

// RecordSuccess closes the breaker again, so a key that failed once and then
// worked is back in the pool without waiting out a backoff.
func TestRecordSuccess_ReturnsTheKeyToThePool(t *testing.T) {
	f := newAcquireFixture(t)
	ctx := context.Background()
	acq, err := f.svc.Acquire(ctx, f.in)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	f.svc.Release(ctx, acq, keypool.FailureAuth, 0)
	if _, err := f.svc.Acquire(ctx, f.in); err == nil {
		t.Fatal("the key is selectable while its breaker is open")
	}
	f.svc.RecordSuccess(ctx, acq)
	if _, err := f.svc.Acquire(ctx, f.in); err != nil {
		t.Fatalf("Acquire after a recorded success: %v", err)
	}
}

// Commit maps the observed token counts onto the rules that asked for them:
// a bare `tokens` rule counts input plus output only, and each `tokens.<key>`
// rule counts its own sub-meter. Summing the whole map would bill a cached,
// reasoning-heavy request several times over.
func TestCommitInbound_TokenMeterMapping(t *testing.T) {
	rules := []appratelimit.Rule{
		{Meter: appratelimit.MeterTokens, Amount: 1 << 30, Window: appratelimit.Window(time.Hour), Strategy: appratelimit.StrategyFixedWindow},
		{Meter: appratelimit.MeterTokensInput, Amount: 1 << 30, Window: appratelimit.Window(time.Hour), Strategy: appratelimit.StrategyFixedWindow},
		{Meter: appratelimit.MeterTokensCacheRead, Amount: 1 << 30, Window: appratelimit.Window(time.Hour), Strategy: appratelimit.StrategyFixedWindow},
		{Meter: appratelimit.MeterTokensReasoning, Amount: 1 << 30, Window: appratelimit.Window(time.Hour), Strategy: appratelimit.StrategyFixedWindow},
	}
	obs := pkgratelimit.Observations{Tokens: map[string]int64{
		"input": 300, "output": 200, "cache_read": 70, "reasoning": 900,
	}}
	// Rule order in the reservation matches the rule order on the RateLimit.
	want := []int64{500, 300, 70, 900}

	t.Run("each meter counts only what it names", func(t *testing.T) {
		svc, store, pol := reserveFixture(t, rules...)
		ctx := context.Background()
		res, err := svc.ReserveInbound(ctx, InboundInput{Policy: pol, TeamID: "team-1"})
		if err != nil {
			t.Fatalf("ReserveInbound: %v", err)
		}
		if err := svc.CommitInbound(ctx, res, obs); err != nil {
			t.Fatalf("CommitInbound: %v", err)
		}
		assertTokenCounters(t, store, want)
	})

	t.Run("a cancelled request counts nothing", func(t *testing.T) {
		svc, store, pol := reserveFixture(t, rules...)
		ctx := context.Background()
		res, err := svc.ReserveInbound(ctx, InboundInput{Policy: pol, TeamID: "team-1"})
		if err != nil {
			t.Fatalf("ReserveInbound: %v", err)
		}
		if err := svc.CommitInbound(ctx, res, pkgratelimit.Observations{Cancelled: true}); err != nil {
			t.Fatalf("CommitInbound: %v", err)
		}
		assertTokenCounters(t, store, []int64{0, 0, 0, 0})
	})

	t.Run("committing twice does not double-count", func(t *testing.T) {
		svc, store, pol := reserveFixture(t, rules...)
		ctx := context.Background()
		res, err := svc.ReserveInbound(ctx, InboundInput{Policy: pol, TeamID: "team-1"})
		if err != nil {
			t.Fatalf("ReserveInbound: %v", err)
		}
		for i := 0; i < 2; i++ {
			if err := svc.CommitInbound(ctx, res, obs); err != nil {
				t.Fatalf("CommitInbound %d: %v", i, err)
			}
		}
		assertTokenCounters(t, store, want)
	})
}

// assertTokenCounters reads the token buckets the last commit touched. The
// commit script's KEYS are [guard, token buckets...] when no rule meters
// concurrency, so the bucket order matches the rule order.
func assertTokenCounters(t *testing.T, store *kvtest.Recording, want []int64) {
	t.Helper()
	var keys []string
	for _, c := range store.Scripts() {
		if c.Name == "limit.commit" {
			keys = c.Keys
		}
	}
	if keys == nil {
		t.Fatal("no commit script ran")
	}
	if len(keys) != len(want)+1 {
		t.Fatalf("commit keys = %v, want the guard plus %d token buckets", keys, len(want))
	}
	for i, w := range want {
		if got := memCounter(t, store, keys[i+1]); got != w {
			t.Errorf("bucket %q = %d, want %d", keys[i+1], got, w)
		}
	}
}

func memCounter(t *testing.T, m kv.Store, key string) int64 {
	t.Helper()
	b, err := m.Get(context.Background(), key)
	if err != nil || len(b) == 0 {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		t.Fatalf("counter %q holds %q, not an integer", key, b)
	}
	return n
}

// A Service with no limiter is the configuration a relay runs with rate
// limiting switched off: nothing reserves, nothing commits, and no path
// dereferences the missing limiter.
func TestReserveInbound_NilLimiterAndCommitPath(t *testing.T) {
	pol := fix("prod-policy")
	pol.Meta.ID = "pol-1"
	rl := testRateLimit("rl-1", requestsRule())
	pol.Spec.RateLimitID = rl.Meta.ID
	svc := NewService(reserveSnap{pol: pol, rl: rl}, nil, nil)
	ctx := context.Background()

	// Rules exist and a token wants its revocation checked; without a
	// limiter neither can be enforced, so nothing is reserved.
	res, err := svc.ReserveInbound(ctx, InboundInput{Policy: pol, TeamID: "team-1", TokenJTI: "jti-1"})
	if err != nil {
		t.Fatalf("ReserveInbound: %v", err)
	}
	if res != nil {
		t.Fatalf("reservation = %+v, want nil with no limiter", res)
	}
	if err := svc.CommitInbound(ctx, res, pkgratelimit.Observations{}); err != nil {
		t.Fatalf("CommitInbound(nil): %v", err)
	}
	if err := svc.CommitBoth(ctx, res, nil, pkgratelimit.Observations{}); err != nil {
		t.Fatalf("CommitBoth: %v", err)
	}
	svc.Release(ctx, &Acquisition{Key: &hostkey.HostKey{}}, keypool.FailureNetwork, 0)

	// A reservation from a configured Service handed to one without a
	// limiter is dropped rather than committed against nothing.
	live, store, livePol := reserveFixture(t, requestsRule())
	got, err := live.ReserveInbound(ctx, InboundInput{Policy: livePol, TeamID: "team-1"})
	if err != nil {
		t.Fatalf("live ReserveInbound: %v", err)
	}
	before := len(store.Scripts())
	if err := svc.CommitInbound(ctx, got, pkgratelimit.Observations{}); err != nil {
		t.Fatalf("CommitInbound without a limiter: %v", err)
	}
	if after := len(store.Scripts()); after != before {
		t.Fatalf("script calls %d → %d, want none from a limiterless commit", before, after)
	}
}

// CommitBoth returns the inbound and upstream reservations together — the
// single round trip the success path is built on — and stays correct when
// only one of them exists.
func TestCommitBoth_ReturnsBothReservationsInOneCall(t *testing.T) {
	f := newAcquireFixture(t, requestsRule())
	ctx := context.Background()
	caller := f.in.Policy
	caller.Spec.RateLimitID = "tier-rl"

	inbound, err := f.svc.ReserveInbound(ctx, InboundInput{Policy: caller, TeamID: "team-1"})
	if err != nil {
		t.Fatalf("ReserveInbound: %v", err)
	}
	if inbound == nil {
		t.Fatal("no inbound reservation to commit")
	}
	acq, err := f.svc.Acquire(ctx, f.in)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := f.svc.CommitBoth(ctx, inbound, acq, pkgratelimit.Observations{}); err != nil {
		t.Fatalf("CommitBoth: %v", err)
	}
	// The two reservations live under different hash tags, so they commit as
	// two individually-atomic scripts — batched, not merged. Counting the
	// scripts alone cannot tell that apart from two sequential commits, which
	// is the round trip this method exists to save, so the batch is what the
	// assertion is on.
	batches, size := batchStats(f.store)
	if batches != 1 {
		t.Fatalf("batched round trips = %d, want exactly 1", batches)
	}
	if size != 2 {
		t.Fatalf("the batch carried %d scripts, want one per reservation", size)
	}
	if got := len(f.store.Named("limit.commit")); got != 2 {
		t.Fatalf("commit scripts = %d, want one per reservation", got)
	}

	// Neither present is a no-op; one present falls back to a single commit,
	// which must not open a batch at all.
	if err := f.svc.CommitBoth(ctx, nil, nil, pkgratelimit.Observations{}); err != nil {
		t.Fatalf("CommitBoth(nil, nil): %v", err)
	}
	if got, _ := batchStats(f.store); got != batches {
		t.Fatalf("batched round trips = %d after a no-op CommitBoth, want %d", got, batches)
	}
	single, err := f.svc.ReserveInbound(ctx, InboundInput{Policy: caller, TeamID: "team-2"})
	if err != nil {
		t.Fatalf("second ReserveInbound: %v", err)
	}
	before := len(f.store.Named("limit.commit"))
	if err := f.svc.CommitBoth(ctx, single, nil, pkgratelimit.Observations{}); err != nil {
		t.Fatalf("CommitBoth with only an inbound reservation: %v", err)
	}
	if got := len(f.store.Named("limit.commit")) - before; got != 1 {
		t.Fatalf("commit scripts = %d, want exactly 1", got)
	}
	if got, _ := batchStats(f.store); got != batches {
		t.Fatalf("batched round trips = %d for a single reservation, want %d", got, batches)
	}
}
