package ratelimit

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
)

// CommitBoth batches two commit scripts that live under different hash tags; it must leave exactly what two sequential Commit calls leave.

// scripterOnly wraps a Scripter but deliberately does NOT expose
// RunScriptBatch, forcing CommitBoth down its sequential fallback path.
type scripterOnly struct{ inner kv.Scripter }

func (s scripterOnly) RunScript(ctx context.Context, name, script string, keys []string, args ...any) ([]byte, error) {
	return s.inner.RunScript(ctx, name, script, keys, args...)
}

// dumpBuckets returns every non-guard key/value in the store, normalized for
// comparison. Guard keys (…:committed:<randomID>) are excluded because their
// name and value carry the per-run random reservation ID; their presence is
// asserted separately by the idempotency test.
func dumpBuckets(t *testing.T, s *kv.Mem) map[string]string {
	t.Helper()
	entries, err := s.Range(context.Background(), "")
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		if strings.Contains(e.Key, ":committed:") {
			continue
		}
		out[e.Key] = string(e.Value)
	}
	return out
}

func countGuards(t *testing.T, s *kv.Mem) int {
	t.Helper()
	entries, err := s.Range(context.Background(), "")
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	n := 0
	for _, e := range entries {
		if strings.Contains(e.Key, ":committed:") {
			n++
		}
	}
	return n
}

const (
	inboundScope  = "prod-policy"
	upstreamScope = "hostkey:key-abc-123"
)

// commitModes are the ways to commit an inbound and an upstream reservation; all must leave the same counters.
var commitModes = []struct {
	name   string
	commit func(l *Limiter, inbound, upstream *Reservation, obs Observations) error
}{
	{"sequential", func(l *Limiter, inbound, upstream *Reservation, obs Observations) error {
		ctx := context.Background()
		return errors.Join(l.Commit(ctx, inbound, obs), l.Commit(ctx, upstream, obs))
	}},
	{"both", func(l *Limiter, inbound, upstream *Reservation, obs Observations) error {
		return l.CommitBoth(context.Background(), inbound, upstream, obs)
	}},
	{"both without a batching store", func(l *Limiter, inbound, upstream *Reservation, obs Observations) error {
		l.runner = scripterOnly{inner: l.runner}
		return l.CommitBoth(context.Background(), inbound, upstream, obs)
	}},
}

// commitDump reserves inbound and upstream rules (nil rules reserve nothing), moves the clock to commitNow, commits through commit and returns the counters left.
func commitDump(t *testing.T, commit func(*Limiter, *Reservation, *Reservation, Observations) error, inboundRules, upstreamRules []Rule, obs Observations, reserveNow, commitNow time.Time) map[string]string {
	t.Helper()
	store := newTestStore(t)
	now := reserveNow
	l := New(store, discardLog(), func() time.Time { return now })
	var inbound, upstream *Reservation
	if inboundRules != nil {
		inbound = mustReserve(t, l, inboundScope, inboundRules)
	}
	if upstreamRules != nil {
		upstream = mustReserve(t, l, upstreamScope, upstreamRules)
	}
	now = commitNow
	if err := commit(l, inbound, upstream, obs); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return dumpBuckets(t, store)
}

func TestCommitBothParity(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC)
	window := time.Minute

	cases := []struct {
		name          string
		inbound       []Rule
		upstream      []Rule
		obs           Observations
		commitAdvance time.Duration
	}{
		{
			name: "requests+tokens+concurrency committed",
			inbound: []Rule{
				reqRule("inb:req", "inbound requests", 100, window),
				tokRule("inb:tok", "inbound tokens", 1_000_000, window),
				conRule("inb:con", "inbound concurrency", 10, window),
			},
			upstream: []Rule{
				reqRule("ups:req", "upstream requests", 100, window),
				tokRule("ups:tok", "upstream tokens", 1_000_000, window),
				conRule("ups:con", "upstream concurrency", 10, window),
			},
			obs: Observations{Tokens: map[string]int64{"input": 300, "output": 200}},
		},
		{
			name: "zero tokens",
			inbound: []Rule{
				reqRule("inb:req", "inbound requests", 100, window),
				tokRule("inb:tok", "inbound tokens", 1_000_000, window),
			},
			upstream: []Rule{
				tokRule("ups:tok", "upstream tokens", 1_000_000, window),
				conRule("ups:con", "upstream concurrency", 10, window),
			},
			obs: Observations{Tokens: map[string]int64{}},
		},
		{
			name: "nil tokens map",
			inbound: []Rule{
				conRule("inb:con", "inbound concurrency", 10, window),
			},
			upstream: []Rule{
				conRule("ups:con", "upstream concurrency", 10, window),
			},
			obs: Observations{},
		},
		{
			name: "cancelled with token-bucket + concurrency refund",
			inbound: []Rule{
				{Key: "inb:req", Name: "inbound requests", Meter: "requests", Strategy: StrategyTokenBucket, Amount: 100, Window: window},
				conRule("inb:con", "inbound concurrency", 10, window),
			},
			upstream: []Rule{
				{Key: "ups:req", Name: "upstream requests", Meter: "requests", Strategy: StrategyLeakyBucket, Amount: 100, Window: window},
				conRule("ups:con", "upstream concurrency", 10, window),
			},
			obs: Observations{Tokens: map[string]int64{"input": 999}, Cancelled: true},
		},
		{
			name: "session-window cancelled refund",
			inbound: []Rule{
				{Key: "inb:sess", Name: "inbound session", Meter: "requests", Strategy: StrategySessionWindow, Amount: 5, Window: window},
			},
			upstream: []Rule{
				{Key: "ups:sess", Name: "upstream session", Meter: "requests", Strategy: StrategySessionWindow, Amount: 5, Window: window},
			},
			obs: Observations{Cancelled: true},
		},
		{
			name: "expired buckets — commit after 3 windows",
			inbound: []Rule{
				reqRule("inb:req", "inbound requests", 100, window),
				tokRule("inb:tok", "inbound tokens", 1_000_000, window),
			},
			upstream: []Rule{
				reqRule("ups:req", "upstream requests", 100, window),
				tokRule("ups:tok", "upstream tokens", 1_000_000, window),
			},
			obs:           Observations{Tokens: map[string]int64{"input": 111, "output": 222}},
			commitAdvance: 3 * window,
		},
		{
			name:    "inbound only",
			inbound: []Rule{reqRule("inb:req", "inbound requests", 100, window), tokRule("inb:tok", "inbound tokens", 1_000_000, window)},
			obs:     Observations{Tokens: map[string]int64{"input": 300, "output": 200}},
		},
		{
			name:     "upstream only",
			upstream: []Rule{conRule("ups:con", "upstream concurrency", 10, window), tokRule("ups:tok", "upstream tokens", 1_000_000, window)},
			obs:      Observations{Tokens: map[string]int64{"input": 300, "output": 200}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			commitNow := base.Add(tc.commitAdvance)
			want := commitDump(t, commitModes[0].commit, tc.inbound, tc.upstream, tc.obs, base, commitNow)
			for _, mode := range commitModes[1:] {
				got := commitDump(t, mode.commit, tc.inbound, tc.upstream, tc.obs, base, commitNow)
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("%s (-sequential +got):\n%s", mode.name, diff)
				}
			}
		})
	}
}

// TestCommitBothIdempotent: a duplicate CommitBoth is a no-op for both
// reservations, exactly like a duplicate sequential Commit pair. One guard key
// per reservation is written and the second call changes nothing.
func TestCommitBothIdempotent(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC)
	window := time.Minute
	store := newTestStore(t)
	now := base
	l := New(store, discardLog(), func() time.Time { return now })

	inbound := []Rule{reqRule("inb:req", "inbound requests", 100, window), tokRule("inb:tok", "inbound tokens", 1_000_000, window)}
	upstream := []Rule{reqRule("ups:req", "upstream requests", 100, window), conRule("ups:con", "upstream concurrency", 10, window)}
	obs := Observations{Tokens: map[string]int64{"input": 300}}

	inb, err := l.Reserve(context.Background(), inboundScope, inbound)
	if err != nil {
		t.Fatalf("reserve inbound: %v", err)
	}
	ups, err := l.Reserve(context.Background(), upstreamScope, upstream)
	if err != nil {
		t.Fatalf("reserve upstream: %v", err)
	}

	if err := l.CommitBoth(context.Background(), inb, ups, obs); err != nil {
		t.Fatalf("CommitBoth #1: %v", err)
	}
	afterFirst := dumpBuckets(t, store)
	if g := countGuards(t, store); g != 2 {
		t.Fatalf("guard count after first commit = %d, want 2 (one per reservation)", g)
	}

	// Second commit with different observed tokens must be ignored (guarded).
	if err := l.CommitBoth(context.Background(), inb, ups, Observations{Tokens: map[string]int64{"input": 9999}}); err != nil {
		t.Fatalf("CommitBoth #2: %v", err)
	}
	if diff := cmp.Diff(afterFirst, dumpBuckets(t, store)); diff != "" {
		t.Fatalf("duplicate commit changed counters (-first +second):\n%s", diff)
	}
	if g := countGuards(t, store); g != 2 {
		t.Fatalf("guard count after duplicate commit = %d, want 2", g)
	}
}

// TestCommitBothConcurrent: N goroutines racing to CommitBoth the same pair of
// reservations must land exactly one effective commit each side (the guard
// serializes duplicates). Run under -race to catch data races in the batch
// plumbing. Concurrency token = 8 (well above amount), so the concurrency
// counters must net to a single +1/-1 = 0 regardless of interleaving.
func TestCommitBothConcurrent(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC)
	window := time.Minute
	store := newTestStore(t)
	now := base
	l := New(store, discardLog(), func() time.Time { return now })

	inbound := []Rule{reqRule("inb:req", "inbound requests", 100, window), tokRule("inb:tok", "inbound tokens", 1_000_000, window)}
	upstream := []Rule{conRule("ups:con", "upstream concurrency", 10, window), tokRule("ups:tok", "upstream tokens", 1_000_000, window)}
	obs := Observations{Tokens: map[string]int64{"input": 300, "output": 200}}

	inb, err := l.Reserve(context.Background(), inboundScope, inbound)
	if err != nil {
		t.Fatalf("reserve inbound: %v", err)
	}
	ups, err := l.Reserve(context.Background(), upstreamScope, upstream)
	if err != nil {
		t.Fatalf("reserve upstream: %v", err)
	}

	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if err := l.CommitBoth(context.Background(), inb, ups, obs); err != nil {
				t.Errorf("CommitBoth: %v", err)
			}
		}()
	}
	wg.Wait()

	dump := dumpBuckets(t, store)
	// Exactly one commit's worth of tokens (500) on the inbound token bucket.
	cur, _ := windowBuckets(base, window)
	tokKey := bucketKey(inboundScope, tokRule("inb:tok", "inbound tokens", 1_000_000, window), cur)
	if got := dump[tokKey]; got != "500" {
		t.Errorf("inbound token bucket %q = %q, want 500 (single effective commit)", tokKey, got)
	}
	if g := countGuards(t, store); g != 2 {
		t.Errorf("guard count = %d, want 2 after concurrent duplicate commits", g)
	}
}

func TestCommitBothOfNothingIsNoOp(t *testing.T) {
	rec := kvtest.NewRecording(newTestStore(t))
	l := New(rec, discardLog(), nil)
	if err := l.CommitBoth(context.Background(), nil, nil, Observations{Cancelled: true}); err != nil {
		t.Fatalf("CommitBoth(nil, nil): %v", err)
	}
	if n := rec.RoundTrips(); n != 0 {
		t.Fatalf("round trips = %d, want 0", n)
	}
}
