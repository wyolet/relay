package ratelimit

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

// commitboth_test.go — parity gate for CommitBoth.
//
// CommitBoth batches the two per-reservation commit scripts into one round
// trip instead of committing inbound then upstream sequentially. The two
// reservations live under different hash tags (policy scope vs. hostkey scope),
// so a single CROSSSLOT-safe Lua is impossible; the guarantee we hold instead
// is EXACT semantic parity with the two sequential Commit calls. These tests
// run each scenario both ways against fresh stores and assert the resulting
// bucket state is byte-identical (modulo the random-ID idempotency guard keys).

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

// commitMode runs the commit half of a scenario against an already-reserved
// pair, either sequentially or via CommitBoth.
type commitMode func(t *testing.T, l *Limiter, inbound, upstream *Reservation, obs Observations)

func sequentialCommit(t *testing.T, l *Limiter, inbound, upstream *Reservation, obs Observations) {
	t.Helper()
	if err := l.Commit(context.Background(), inbound, obs); err != nil {
		t.Fatalf("sequential inbound commit: %v", err)
	}
	if err := l.Commit(context.Background(), upstream, obs); err != nil {
		t.Fatalf("sequential upstream commit: %v", err)
	}
}

func batchedCommit(t *testing.T, l *Limiter, inbound, upstream *Reservation, obs Observations) {
	t.Helper()
	if err := l.CommitBoth(context.Background(), inbound, upstream, obs); err != nil {
		t.Fatalf("CommitBoth: %v", err)
	}
}

// runParityScenario reserves inbound+upstream, advances the clock to commitNow,
// commits via mode, and returns the resulting non-guard bucket dump.
func runParityScenario(
	t *testing.T,
	mode commitMode,
	inboundRules, upstreamRules []Rule,
	obs Observations,
	reserveNow, commitNow time.Time,
) map[string]string {
	t.Helper()
	store := newTestStore(t)
	now := reserveNow
	l := New(store, discardLog(), func() time.Time { return now })

	inbound, err := l.Reserve(context.Background(), inboundScope, inboundRules)
	if err != nil {
		t.Fatalf("reserve inbound: %v", err)
	}
	upstream, err := l.Reserve(context.Background(), upstreamScope, upstreamRules)
	if err != nil {
		t.Fatalf("reserve upstream: %v", err)
	}

	now = commitNow
	mode(t, l, inbound, upstream, obs)
	return dumpBuckets(t, store)
}

func assertMapsEqual(t *testing.T, seq, batch map[string]string) {
	t.Helper()
	if len(seq) != len(batch) {
		t.Errorf("key count: sequential=%d batched=%d", len(seq), len(batch))
	}
	keys := make([]string, 0, len(seq))
	for k := range seq {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if batch[k] != seq[k] {
			t.Errorf("key %q: sequential=%q batched=%q", k, seq[k], batch[k])
		}
	}
	for k := range batch {
		if _, ok := seq[k]; !ok {
			t.Errorf("key %q present only in batched=%q", k, batch[k])
		}
	}
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
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			commitNow := base.Add(tc.commitAdvance)
			seq := runParityScenario(t, sequentialCommit, tc.inbound, tc.upstream, tc.obs, base, commitNow)
			batch := runParityScenario(t, batchedCommit, tc.inbound, tc.upstream, tc.obs, base, commitNow)
			assertMapsEqual(t, seq, batch)
		})
	}
}

// TestCommitBothFallbackParity: a runner without RunScriptBatch must produce
// the same result as the batched path (the sequential fallback).
func TestCommitBothFallbackParity(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC)
	window := time.Minute
	inbound := []Rule{reqRule("inb:req", "inbound requests", 100, window), tokRule("inb:tok", "inbound tokens", 1_000_000, window)}
	upstream := []Rule{reqRule("ups:req", "upstream requests", 100, window), conRule("ups:con", "upstream concurrency", 10, window)}
	obs := Observations{Tokens: map[string]int64{"input": 300, "output": 200}}

	// Batched path.
	batch := runParityScenario(t, batchedCommit, inbound, upstream, obs, base, base)

	// Fallback path: same scenario, but force runner to a non-batch Scripter.
	store := newTestStore(t)
	now := base
	l := New(store, discardLog(), func() time.Time { return now })
	l.runner = scripterOnly{inner: store}

	inb, err := l.Reserve(context.Background(), inboundScope, inbound)
	if err != nil {
		t.Fatalf("reserve inbound: %v", err)
	}
	ups, err := l.Reserve(context.Background(), upstreamScope, upstream)
	if err != nil {
		t.Fatalf("reserve upstream: %v", err)
	}
	if err := l.CommitBoth(context.Background(), inb, ups, obs); err != nil {
		t.Fatalf("CommitBoth (fallback): %v", err)
	}
	fallback := dumpBuckets(t, store)
	assertMapsEqual(t, fallback, batch)
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
	afterSecond := dumpBuckets(t, store)
	assertMapsEqual(t, afterFirst, afterSecond)
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

// TestCommitBothSingleSided: with one reservation nil, CommitBoth must match a
// single Commit of the present side (fallback-to-single path).
func TestCommitBothSingleSided(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC)
	window := time.Minute
	rules := []Rule{reqRule("inb:req", "inbound requests", 100, window), tokRule("inb:tok", "inbound tokens", 1_000_000, window)}
	obs := Observations{Tokens: map[string]int64{"input": 300, "output": 200}}

	// Single Commit reference.
	refStore := newTestStore(t)
	now := base
	lref := New(refStore, discardLog(), func() time.Time { return now })
	res, err := lref.Reserve(context.Background(), inboundScope, rules)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := lref.Commit(context.Background(), res, obs); err != nil {
		t.Fatalf("commit: %v", err)
	}
	ref := dumpBuckets(t, refStore)

	// CommitBoth with upstream nil.
	store := newTestStore(t)
	now2 := base
	l := New(store, discardLog(), func() time.Time { return now2 })
	r, err := l.Reserve(context.Background(), inboundScope, rules)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := l.CommitBoth(context.Background(), r, nil, obs); err != nil {
		t.Fatalf("CommitBoth single: %v", err)
	}
	got := dumpBuckets(t, store)
	assertMapsEqual(t, ref, got)

	// Both nil is a clean no-op.
	if err := l.CommitBoth(context.Background(), nil, nil, obs); err != nil {
		t.Fatalf("CommitBoth(nil,nil): %v", err)
	}
}
