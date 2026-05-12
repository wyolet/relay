package keypool

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/pkg/kv"
)

// helpers

func newSel(t *testing.T, clock func() time.Time) (*Selector, *kv.Mem) {
	t.Helper()
	ms := kv.NewMem()
	t.Cleanup(func() { ms.Close() })
	return New(ms, noopLogger(), clock, nil), ms
}

func noopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func secret(name, hash string) *catalog.Secret {
	return &catalog.Secret{
		Metadata: catalog.Metadata{Name: name},
		KeyHash:  hash,
	}
}

func pool(name string) *catalog.Policy {
	return &catalog.Policy{Metadata: catalog.Metadata{Name: name}}
}

func poolWithStrategy(name string, strategy catalog.KeySelection) *catalog.Policy {
	return &catalog.Policy{
		Metadata: catalog.Metadata{Name: name},
		Spec:     catalog.PolicySpec{KeySelection: strategy},
	}
}

// frozen clock helpers

func frozenClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func advancedClock(base time.Time, delta time.Duration) func() time.Time {
	return frozenClock(base.Add(delta))
}

var _ = advancedClock // suppress unused warning

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// TestRecordSuccess — fresh key, success recorded → closed + BackoffStep=0.
func TestRecordSuccess(t *testing.T) {
	sel, ms := newSel(t, frozenClock(t0))
	_ = ms
	ctx := context.Background()
	sel.RecordSuccess(ctx, "hash1")
	rec := sel.readRecord(ctx, "hash1")
	if rec.State != CircuitClosed {
		t.Fatalf("want closed, got %v", rec.State)
	}
	if rec.BackoffStep != 0 {
		t.Fatalf("want BackoffStep=0, got %d", rec.BackoffStep)
	}
}

// TestAuthFailureIsIndefinite — auth failure → open+indefinite; Pick returns ErrNoHealthyKeys.
func TestAuthFailureIsIndefinite(t *testing.T) {
	sel, _ := newSel(t, frozenClock(t0))
	ctx := context.Background()
	k := "hash-auth"
	sel.RecordFailure(ctx, k, FailureAuth, 0)
	rec := sel.readRecord(ctx, k)
	if rec.State != CircuitOpen || !rec.Indefinite {
		t.Fatal("want open+indefinite")
	}
	p := pool("p")
	_, err := sel.Pick(ctx, nil, p, nil, []*catalog.Secret{secret("s", k)})
	if err != ErrNoHealthyKeys {
		t.Fatalf("want ErrNoHealthyKeys, got %v", err)
	}
}

// TestRateLimitShortStaysClosed — short rate-limit → state unchanged; Pick returns key.
func TestRateLimitShortStaysClosed(t *testing.T) {
	sel, _ := newSel(t, frozenClock(t0))
	ctx := context.Background()
	k := "hash-rls"
	sel.RecordFailure(ctx, k, FailureRateLimitShort, 2*time.Second)
	rec := sel.readRecord(ctx, k)
	if rec.State != CircuitClosed {
		t.Fatalf("want closed, got %v", rec.State)
	}
	p := pool("p2")
	got, err := sel.Pick(ctx, nil, p, nil, []*catalog.Secret{secret("s", k)})
	if err != nil {
		t.Fatal(err)
	}
	if got.KeyHash != k {
		t.Fatalf("wrong key returned")
	}
}

// TestRateLimitLongOpensForDuration — long RL opens until t+30s; skipped at t+10s; half-open at t+31s.
func TestRateLimitLongOpensForDuration(t *testing.T) {
	sel, ms := newSel(t, frozenClock(t0))
	_ = ms
	ctx := context.Background()
	k := "hash-rll"
	sel.RecordFailure(ctx, k, FailureRateLimitLong, 30*time.Second)

	// at t+10s — still open
	sel.clock = frozenClock(t0.Add(10 * time.Second))
	p := pool("p3")
	_, err := sel.Pick(ctx, nil, p, nil, []*catalog.Secret{secret("s", k)})
	if err != ErrNoHealthyKeys {
		t.Fatalf("want ErrNoHealthyKeys at t+10s, got %v", err)
	}

	// at t+31s — should auto-transition to half-open and be eligible
	sel.clock = frozenClock(t0.Add(31 * time.Second))
	got, err := sel.Pick(ctx, nil, p, nil, []*catalog.Secret{secret("s", k)})
	if err != nil {
		t.Fatalf("want key at t+31s, got err %v", err)
	}
	if got.KeyHash != k {
		t.Fatal("wrong key")
	}
	rec := sel.readRecord(ctx, k)
	if rec.State != CircuitHalfOpen {
		t.Fatalf("want half-open after pick, got %v", rec.State)
	}
}

// TestServerErrorBackoffEscalates — three consecutive 5xx → BackoffStep grows 1→2→3.
func TestServerErrorBackoffEscalates(t *testing.T) {
	sel, _ := newSel(t, frozenClock(t0))
	ctx := context.Background()
	k := "hash-5xx"

	for i, wantStep := range []int{1, 2, 3} {
		sel.RecordFailure(ctx, k, FailureServerError, 0)
		rec := sel.readRecord(ctx, k)
		if rec.BackoffStep != wantStep {
			t.Fatalf("iter %d: want BackoffStep=%d, got %d", i, wantStep, rec.BackoffStep)
		}
	}
	// Duration at step 3 = 8s.
	rec := sel.readRecord(ctx, k)
	wantUntil := t0.Add(8 * time.Second)
	if rec.OpenUntil != wantUntil {
		t.Fatalf("want OpenUntil=%v, got %v", wantUntil, rec.OpenUntil)
	}

	// Past OpenUntil → half-open probe; record success → BackoffStep=0.
	sel.clock = frozenClock(t0.Add(9 * time.Second))
	p := pool("p4")
	got, err := sel.Pick(ctx, nil, p, nil, []*catalog.Secret{secret("s", k)})
	if err != nil || got.KeyHash != k {
		t.Fatal("expected half-open pick")
	}
	sel.RecordSuccess(ctx, k)
	rec = sel.readRecord(ctx, k)
	if rec.BackoffStep != 0 || rec.State != CircuitClosed {
		t.Fatalf("after success: want closed/step=0, got state=%v step=%d", rec.State, rec.BackoffStep)
	}
}

// TestNetworkBehavesLike5xx — network failure follows same backoff schedule.
func TestNetworkBehavesLike5xx(t *testing.T) {
	sel, _ := newSel(t, frozenClock(t0))
	ctx := context.Background()
	k := "hash-net"
	sel.RecordFailure(ctx, k, FailureNetwork, 0)
	rec := sel.readRecord(ctx, k)
	if rec.State != CircuitOpen || rec.BackoffStep != 1 {
		t.Fatalf("want open/step=1, got %v/%d", rec.State, rec.BackoffStep)
	}
	wantUntil := t0.Add(time.Duration(backoffSchedule[1]) * time.Second)
	if rec.OpenUntil != wantUntil {
		t.Fatalf("want OpenUntil=%v, got %v", wantUntil, rec.OpenUntil)
	}
}

// TestPick_RoundRobin — three healthy keys; 30 picks → ~10 each (±1).
func TestPick_RoundRobin(t *testing.T) {
	sel, _ := newSel(t, frozenClock(t0))
	ctx := context.Background()
	secrets := []*catalog.Secret{
		secret("a", "hA"),
		secret("b", "hB"),
		secret("c", "hC"),
	}
	p := poolWithStrategy("rr", catalog.KeySelectionRoundRobin)
	counts := map[string]int{}
	for i := 0; i < 30; i++ {
		got, err := sel.Pick(ctx, nil, p, nil, secrets)
		if err != nil {
			t.Fatal(err)
		}
		counts[got.KeyHash]++
	}
	for _, k := range []string{"hA", "hB", "hC"} {
		if counts[k] < 9 || counts[k] > 11 {
			t.Fatalf("uneven distribution: %v", counts)
		}
	}
}

// TestPick_SkipsOpen — one auth-failed key; picks distributed across other two.
func TestPick_SkipsOpen(t *testing.T) {
	sel, _ := newSel(t, frozenClock(t0))
	ctx := context.Background()
	sel.RecordFailure(ctx, "hB", FailureAuth, 0)
	secrets := []*catalog.Secret{
		secret("a", "hA"),
		secret("b", "hB"),
		secret("c", "hC"),
	}
	p := poolWithStrategy("skip", catalog.KeySelectionRoundRobin)
	for i := 0; i < 20; i++ {
		got, err := sel.Pick(ctx, nil, p, nil, secrets)
		if err != nil {
			t.Fatal(err)
		}
		if got.KeyHash == "hB" {
			t.Fatal("picked open key hB")
		}
	}
}

// TestPick_NoHealthy — all auth-failed → ErrNoHealthyKeys.
func TestPick_NoHealthy(t *testing.T) {
	sel, _ := newSel(t, frozenClock(t0))
	ctx := context.Background()
	secrets := []*catalog.Secret{
		secret("a", "hA"),
		secret("b", "hB"),
	}
	for _, sec := range secrets {
		sel.RecordFailure(ctx, sec.KeyHash, FailureAuth, 0)
	}
	p := pool("none")
	_, err := sel.Pick(ctx, nil, p, nil, secrets)
	if err != ErrNoHealthyKeys {
		t.Fatalf("want ErrNoHealthyKeys, got %v", err)
	}
}

// TestPick_HalfOpenOnceVisible — open key past OpenUntil → Pick returns it; no panic on second Pick.
func TestPick_HalfOpenOnceVisible(t *testing.T) {
	sel, _ := newSel(t, frozenClock(t0))
	ctx := context.Background()
	k := "hash-ho"
	sel.RecordFailure(ctx, k, FailureRateLimitLong, 5*time.Second)
	sel.clock = frozenClock(t0.Add(10 * time.Second))
	p := pool("ho")
	sec := secret("s", k)

	got, err := sel.Pick(ctx, nil, p, nil, []*catalog.Secret{sec})
	if err != nil || got.KeyHash != k {
		t.Fatalf("first pick: want key, got err=%v", err)
	}
	// Second pick without recording outcome — should not panic.
	_, err2 := sel.Pick(ctx, nil, p, nil, []*catalog.Secret{sec})
	_ = err2 // half-open may be returned or not; just no panic
}

// TestPickConcurrent — 100 goroutines pick from a 3-key healthy pool; all 3 get hits.
func TestPickConcurrent(t *testing.T) {
	sel, _ := newSel(t, frozenClock(t0))
	ctx := context.Background()
	secrets := []*catalog.Secret{
		secret("a", "cA"),
		secret("b", "cB"),
		secret("c", "cC"),
	}
	p := poolWithStrategy("concurrent", catalog.KeySelectionRoundRobin)
	var (
		mu     sync.Mutex
		counts = map[string]int{}
		wg     sync.WaitGroup
		errCnt atomic.Int64
	)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := sel.Pick(ctx, nil, p, nil, secrets)
			if err != nil {
				errCnt.Add(1)
				return
			}
			mu.Lock()
			counts[got.KeyHash]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if errCnt.Load() > 0 {
		t.Fatalf("%d concurrent picks returned errors", errCnt.Load())
	}
	for _, k := range []string{"cA", "cB", "cC"} {
		if counts[k] == 0 {
			t.Fatalf("key %s got zero picks: %v", k, counts)
		}
	}
}

// TestRecordSuccessClearsBackoff — after a 5xx series, RecordSuccess resets BackoffStep to 0.
func TestRecordSuccessClearsBackoff(t *testing.T) {
	sel, _ := newSel(t, frozenClock(t0))
	ctx := context.Background()
	k := "hash-clr"
	for i := 0; i < 4; i++ {
		sel.RecordFailure(ctx, k, FailureServerError, 0)
	}
	rec := sel.readRecord(ctx, k)
	if rec.BackoffStep == 0 {
		t.Fatal("expected non-zero backoff after failures")
	}
	sel.RecordSuccess(ctx, k)
	rec = sel.readRecord(ctx, k)
	if rec.BackoffStep != 0 || rec.State != CircuitClosed {
		t.Fatalf("after RecordSuccess: want closed/0, got %v/%d", rec.State, rec.BackoffStep)
	}
}

// ── Strategy tests ─────────────────────────────────────────────────────────────

// TestPick_Prioritized_HealthyFirst — A, B, C all healthy → A always picked.
func TestPick_Prioritized_HealthyFirst(t *testing.T) {
	sel, _ := newSel(t, frozenClock(t0))
	ctx := context.Background()
	secrets := []*catalog.Secret{
		secret("a", "hA"),
		secret("b", "hB"),
		secret("c", "hC"),
	}
	p := poolWithStrategy("pri", catalog.KeySelectionPrioritized)
	for i := 0; i < 10; i++ {
		got, err := sel.Pick(ctx, nil, p, nil, secrets)
		if err != nil {
			t.Fatal(err)
		}
		if got.KeyHash != "hA" {
			t.Fatalf("prioritized: want hA, got %s", got.KeyHash)
		}
	}
}

// TestPick_Prioritized_SkipsOpenFirst — A in cooldown → B.
func TestPick_Prioritized_SkipsOpenFirst(t *testing.T) {
	sel, _ := newSel(t, frozenClock(t0))
	ctx := context.Background()
	sel.RecordFailure(ctx, "hA", FailureAuth, 0)
	secrets := []*catalog.Secret{
		secret("a", "hA"),
		secret("b", "hB"),
		secret("c", "hC"),
	}
	p := poolWithStrategy("pri2", catalog.KeySelectionPrioritized)
	got, err := sel.Pick(ctx, nil, p, nil, secrets)
	if err != nil {
		t.Fatal(err)
	}
	if got.KeyHash != "hB" {
		t.Fatalf("prioritized with A open: want hB, got %s", got.KeyHash)
	}
}

// TestPick_LRU_PrefersNeverUsed — A used at t=1, B at t=2, C never → C.
func TestPick_LRU_PrefersNeverUsed(t *testing.T) {
	ms := kv.NewMem()
	defer ms.Close()
	ctx := context.Background()

	clk := frozenClock(t0)
	sel := New(ms, noopLogger(), clk, nil)
	p := poolWithStrategy("lru1", catalog.KeySelectionLeastRecentlyUsed)

	secA := secret("a", "hA")
	secB := secret("b", "hB")
	secC := secret("c", "hC")

	// Stamp A at t0, B at t0+1s.
	sel.clock = frozenClock(t0)
	_, _ = sel.Pick(ctx, nil, poolWithStrategy("lru1", catalog.KeySelectionLeastRecentlyUsed), nil, []*catalog.Secret{secA})
	sel.clock = frozenClock(t0.Add(time.Second))
	_, _ = sel.Pick(ctx, nil, poolWithStrategy("lru1", catalog.KeySelectionLeastRecentlyUsed), nil, []*catalog.Secret{secB})

	// Now pick from all three — C was never used, prefer C.
	sel.clock = frozenClock(t0.Add(2 * time.Second))
	got, err := sel.Pick(ctx, nil, p, nil, []*catalog.Secret{secA, secB, secC})
	if err != nil {
		t.Fatal(err)
	}
	if got.KeyHash != "hC" {
		t.Fatalf("LRU: want hC (never used), got %s", got.KeyHash)
	}
}

// TestPick_LRU_PrefersOldest — A at t=1, B at t=2, C at t=3 → next pick prefers A.
func TestPick_LRU_PrefersOldest(t *testing.T) {
	ms := kv.NewMem()
	defer ms.Close()
	ctx := context.Background()

	sel := New(ms, noopLogger(), frozenClock(t0), nil)
	p := poolWithStrategy("lru2", catalog.KeySelectionLeastRecentlyUsed)
	secA := secret("a", "hA")
	secB := secret("b", "hB")
	secC := secret("c", "hC")

	// Stamp all three at increasing times.
	sel.clock = frozenClock(t0.Add(1 * time.Second))
	_, _ = sel.Pick(ctx, nil, p, nil, []*catalog.Secret{secA})
	sel.clock = frozenClock(t0.Add(2 * time.Second))
	_, _ = sel.Pick(ctx, nil, p, nil, []*catalog.Secret{secB})
	sel.clock = frozenClock(t0.Add(3 * time.Second))
	_, _ = sel.Pick(ctx, nil, p, nil, []*catalog.Secret{secC})

	// Next pick: A has oldest stamp (t0+1s) → prefer A.
	sel.clock = frozenClock(t0.Add(4 * time.Second))
	got, err := sel.Pick(ctx, nil, p, nil, []*catalog.Secret{secA, secB, secC})
	if err != nil {
		t.Fatal(err)
	}
	if got.KeyHash != "hA" {
		t.Fatalf("LRU: want hA (oldest), got %s", got.KeyHash)
	}

	// Now A has been used at t0+4s; next pick prefers B (t0+2s).
	sel.clock = frozenClock(t0.Add(5 * time.Second))
	got2, err := sel.Pick(ctx, nil, p, nil, []*catalog.Secret{secA, secB, secC})
	if err != nil {
		t.Fatal(err)
	}
	if got2.KeyHash != "hB" {
		t.Fatalf("LRU second pick: want hB, got %s", got2.KeyHash)
	}
}

// TestPick_Exclude — secret in exclude list is never returned even if healthy.
func TestPick_Exclude(t *testing.T) {
	sel, _ := newSel(t, frozenClock(t0))
	ctx := context.Background()
	secA := secret("a", "hA")
	secB := secret("b", "hB")
	secC := secret("c", "hC")
	secrets := []*catalog.Secret{secA, secB, secC}
	p := pool("excl")

	for i := 0; i < 20; i++ {
		got, err := sel.Pick(ctx, nil, p, nil, secrets, []*catalog.Secret{secA})
		if err != nil {
			t.Fatal(err)
		}
		if got.KeyHash == "hA" {
			t.Fatal("excluded secret hA was returned")
		}
	}
}

// TestPick_ExcludeAll — excluding all healthy secrets → ErrNoHealthyKeys.
func TestPick_ExcludeAll(t *testing.T) {
	sel, _ := newSel(t, frozenClock(t0))
	ctx := context.Background()
	secA := secret("a", "hA")
	p := pool("excl-all")
	_, err := sel.Pick(ctx, nil, p, nil, []*catalog.Secret{secA}, []*catalog.Secret{secA})
	if err != ErrNoHealthyKeys {
		t.Fatalf("want ErrNoHealthyKeys when all excluded, got %v", err)
	}
}

// TestCooldownReasons — each FailureKind stamps the expected Reason on the circuit record.
func TestCooldownReasons(t *testing.T) {
	cases := []struct {
		name     string
		kind     FailureKind
		retry    time.Duration
		wantRsn  CooldownReason
		wantOpen bool
	}{
		{"auth", FailureAuth, 0, ReasonUpstreamAuthFailed, true},
		{"rl_long", FailureRateLimitLong, 30 * time.Second, ReasonUpstreamRateLimited, true},
		{"server_error", FailureServerError, 0, ReasonUpstreamServerError, true},
		{"network", FailureNetwork, 0, ReasonUpstreamNetworkError, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sel, _ := newSel(t, frozenClock(t0))
			ctx := context.Background()
			h := "hash-" + tc.name
			sel.RecordFailure(ctx, h, tc.kind, tc.retry)
			rec := sel.readRecord(ctx, h)
			if rec.Reason != tc.wantRsn {
				t.Fatalf("want reason %q, got %q", tc.wantRsn, rec.Reason)
			}
			if tc.wantOpen && rec.State != CircuitOpen {
				t.Fatalf("want CircuitOpen, got %v", rec.State)
			}
		})
	}
}

// TestRateLimitShortPreservesReason — short RL does not write a record, so reason stays empty.
func TestRateLimitShortPreservesReason(t *testing.T) {
	sel, _ := newSel(t, frozenClock(t0))
	ctx := context.Background()
	h := "hash-rls-reason"
	sel.RecordFailure(ctx, h, FailureRateLimitShort, 2*time.Second)
	rec := sel.readRecord(ctx, h)
	// No record was written; default is closed with empty reason.
	if rec.Reason != "" {
		t.Fatalf("want empty reason, got %q", rec.Reason)
	}
}

// TestRecordLocalRateLimit — writes CircuitOpen with correct reason; BackoffStep unchanged.
func TestRecordLocalRateLimit(t *testing.T) {
	sel, _ := newSel(t, frozenClock(t0))
	ctx := context.Background()
	k := "reason-local-rl"
	// Pre-warm a backoff step so we can assert it is preserved.
	sel.RecordFailure(ctx, k, FailureServerError, 0)
	before := sel.readRecord(ctx, k)
	wantStep := before.BackoffStep

	sel.RecordLocalRateLimit(ctx, k, 15*time.Second)
	rec := sel.readRecord(ctx, k)
	if rec.State != CircuitOpen {
		t.Fatalf("want CircuitOpen, got %v", rec.State)
	}
	if rec.Reason != ReasonLocalRateLimited {
		t.Fatalf("want reason=%q, got %q", ReasonLocalRateLimited, rec.Reason)
	}
	wantUntil := t0.Add(15 * time.Second)
	if rec.OpenUntil != wantUntil {
		t.Fatalf("want OpenUntil=%v, got %v", wantUntil, rec.OpenUntil)
	}
	if rec.BackoffStep != wantStep {
		t.Fatalf("BackoffStep must not change: want %d, got %d", wantStep, rec.BackoffStep)
	}
	if rec.Indefinite {
		t.Fatal("want Indefinite=false")
	}
}

// TestRecordSuccessClearsReason — after a failure, RecordSuccess resets Reason to "".
func TestRecordSuccessClearsReason(t *testing.T) {
	sel, _ := newSel(t, frozenClock(t0))
	ctx := context.Background()
	k := "reason-clear"
	sel.RecordFailure(ctx, k, FailureAuth, 0)
	rec := sel.readRecord(ctx, k)
	if rec.Reason == "" {
		t.Fatal("expected non-empty reason after auth failure")
	}
	sel.RecordSuccess(ctx, k)
	rec = sel.readRecord(ctx, k)
	if rec.Reason != "" {
		t.Fatalf("want empty reason after success, got %q", rec.Reason)
	}
}

// TestBackwardCompatNoReason — a record encoded without the "reason" field decodes
// cleanly with Reason == "" (backward compat with records written before this field).
func TestBackwardCompatNoReason(t *testing.T) {
	oldJSON := []byte(`{"state":1,"open_until":"2026-01-01T00:00:30Z","backoff_step":2,"last_transition":"2026-01-01T00:00:00Z","indefinite":false}`)
	rec, err := decodeRecord(oldJSON)
	if err != nil {
		t.Fatalf("decodeRecord failed: %v", err)
	}
	if rec.State != CircuitOpen {
		t.Fatalf("want CircuitOpen, got %v", rec.State)
	}
	if rec.BackoffStep != 2 {
		t.Fatalf("want BackoffStep=2, got %d", rec.BackoffStep)
	}
	if rec.Reason != "" {
		t.Fatalf("want empty reason from old record, got %q", rec.Reason)
	}
}

// TestPickWithExclude_Convenience — PickWithExclude wrapper works correctly.
func TestPickWithExclude_Convenience(t *testing.T) {
	sel, _ := newSel(t, frozenClock(t0))
	ctx := context.Background()
	secA := secret("a", "hA")
	secB := secret("b", "hB")
	p := pool("conv")
	got, err := sel.PickWithExclude(ctx, nil, p, nil, []*catalog.Secret{secA, secB}, []*catalog.Secret{secA})
	if err != nil {
		t.Fatal(err)
	}
	if got.KeyHash != "hB" {
		t.Fatalf("want hB, got %s", got.KeyHash)
	}
}
