package keypool

import (
	"context"
	"io"
	"log/slog"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/configstore"
	"github.com/wyolet/relay/pkg/limit"
	"github.com/wyolet/relay/pkg/kv"
)

// helpers

func newSel(t *testing.T, clock func() time.Time) (*Selector, *kv.Mem) {
	t.Helper()
	ms := kv.NewMem()
	t.Cleanup(func() { ms.Close() })
	return New(ms, noopLogger(), clock, nil, nil, nil), ms
}

func noopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func secret(name, hash string) *configstore.Secret {
	return &configstore.Secret{
		Metadata: configstore.Metadata{Name: name},
		KeyHash:  hash,
	}
}

func pool(name string) *configstore.Pool {
	return &configstore.Pool{Metadata: configstore.Metadata{Name: name}}
}

// frozen clock helpers

func frozenClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func advancedClock(base time.Time, delta time.Duration) func() time.Time {
	return frozenClock(base.Add(delta))
}

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
	_, err := sel.Pick(ctx, nil, p, nil, []*configstore.Secret{secret("s", k)})
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
	got, err := sel.Pick(ctx, nil, p, nil, []*configstore.Secret{secret("s", k)})
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
	_, err := sel.Pick(ctx, nil, p, nil, []*configstore.Secret{secret("s", k)})
	if err != ErrNoHealthyKeys {
		t.Fatalf("want ErrNoHealthyKeys at t+10s, got %v", err)
	}

	// at t+31s — should auto-transition to half-open and be eligible
	sel.clock = frozenClock(t0.Add(31 * time.Second))
	got, err := sel.Pick(ctx, nil, p, nil, []*configstore.Secret{secret("s", k)})
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
	got, err := sel.Pick(ctx, nil, p, nil, []*configstore.Secret{secret("s", k)})
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
	secrets := []*configstore.Secret{
		secret("a", "hA"),
		secret("b", "hB"),
		secret("c", "hC"),
	}
	p := pool("rr")
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
	secrets := []*configstore.Secret{
		secret("a", "hA"),
		secret("b", "hB"),
		secret("c", "hC"),
	}
	p := pool("skip")
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
	secrets := []*configstore.Secret{
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

	got, err := sel.Pick(ctx, nil, p, nil, []*configstore.Secret{sec})
	if err != nil || got.KeyHash != k {
		t.Fatalf("first pick: want key, got err=%v", err)
	}
	// Second pick without recording outcome — should not panic.
	_, err2 := sel.Pick(ctx, nil, p, nil, []*configstore.Secret{sec})
	_ = err2 // half-open may be returned or not; just no panic
}

// TestPickConcurrent — 100 goroutines pick from a 3-key healthy pool; all 3 get hits.
func TestPickConcurrent(t *testing.T) {
	sel, _ := newSel(t, frozenClock(t0))
	ctx := context.Background()
	secrets := []*configstore.Secret{
		secret("a", "cA"),
		secret("b", "cB"),
		secret("c", "cC"),
	}
	p := pool("concurrent")
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

// --- Weighted-random tests ---

// stubCfg is a minimal configstore.ConfigStore that returns pre-set rules per secret name.
type stubCfg struct {
	rules map[string][]configstore.ResolvedRule // keyed by secret name
}

func (c *stubCfg) RateLimitsForRequest(_ *configstore.Provider, _ *configstore.Pool, _ *configstore.Model, sec *configstore.Secret) []configstore.ResolvedRule {
	if sec == nil {
		return nil
	}
	return c.rules[sec.Metadata.Name]
}
func (c *stubCfg) ProviderByName(_ string) (*configstore.Provider, bool)  { return nil, false }
func (c *stubCfg) ModelByName(_ string) (*configstore.Model, bool)        { return nil, false }
func (c *stubCfg) RouteByName(_ string) (*configstore.Route, bool)        { return nil, false }
func (c *stubCfg) RateLimitByName(_ string) (*configstore.RateLimit, bool) { return nil, false }
func (c *stubCfg) SecretByName(_ string) (*configstore.Secret, bool)      { return nil, false }
func (c *stubCfg) PoolByName(_ string) (*configstore.Pool, bool)          { return nil, false }
func (c *stubCfg) Providers() []*configstore.Provider                     { return nil }
func (c *stubCfg) Models() []*configstore.Model                           { return nil }
func (c *stubCfg) Routes() []*configstore.Route                           { return nil }
func (c *stubCfg) RateLimits() []*configstore.RateLimit                   { return nil }
func (c *stubCfg) Secrets() []*configstore.Secret                         { return nil }
func (c *stubCfg) Pools() []*configstore.Pool                             { return nil }
func (c *stubCfg) DefaultProvider() *configstore.Provider                 { return nil }
func (c *stubCfg) DefaultRoute() *configstore.Route                       { return nil }
func (c *stubCfg) ProviderForModel(_ string) (*configstore.Provider, bool) { return nil, false }
func (c *stubCfg) SecretsForPool(_ *configstore.Pool) []*configstore.Secret { return nil }

// makeRule creates a ResolvedRule with a given meter and amount.
func makeRule(name string, meter configstore.Meter, amount int64) configstore.ResolvedRule {
	return configstore.ResolvedRule{
		ParentKind: configstore.KindSecret,
		ParentName: name,
		Meter:      meter,
		RateLimit: &configstore.RateLimit{
			Metadata: configstore.Metadata{Name: name + "-" + string(meter)},
			Spec: configstore.RateLimitSpec{
				Strategy: configstore.StrategySlidingWindow,
				Window:   time.Minute,
				Amount:   amount,
			},
		},
	}
}

// newWeightedSel builds a Selector with a limiter and stubCfg.
func newWeightedSel(t *testing.T, cfg *stubCfg, rng *rand.Rand) (*Selector, *limit.Limiter, *kv.Mem) {
	t.Helper()
	ms := kv.NewMem()
	t.Cleanup(func() { ms.Close() })
	l := limit.New(ms, noopLogger(), nil)
	sel := New(ms, noopLogger(), frozenClock(t0), l, cfg, rng)
	return sel, l, ms
}

// TestPickWeighted_SkewsToHigherQuota — 1000 vs 100 quota; over 10000 picks ratio ~10:1 (±20%).
func TestPickWeighted_SkewsToHigherQuota(t *testing.T) {
	cfg := &stubCfg{rules: map[string][]configstore.ResolvedRule{
		"high": {makeRule("high", configstore.MeterRequests, 1000)},
		"low":  {makeRule("low", configstore.MeterRequests, 100)},
	}}
	rng := rand.New(rand.NewSource(42))
	sel, _, _ := newWeightedSel(t, cfg, rng)
	ctx := context.Background()
	p := pool("w1")
	secrets := []*configstore.Secret{
		{Metadata: configstore.Metadata{Name: "high"}, KeyHash: "hHigh"},
		{Metadata: configstore.Metadata{Name: "low"}, KeyHash: "hLow"},
	}
	counts := map[string]int{}
	const n = 10000
	for i := 0; i < n; i++ {
		got, err := sel.Pick(ctx, nil, p, nil, secrets)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		counts[got.Metadata.Name]++
	}
	ratio := float64(counts["high"]) / float64(counts["low"])
	if ratio < 8.0 || ratio > 12.0 {
		t.Fatalf("expected ~10:1 ratio, got high=%d low=%d ratio=%.2f", counts["high"], counts["low"], ratio)
	}
}

// TestPickWeighted_ZeroQuotaSkipped — three secrets, one at zero; it's never picked.
func TestPickWeighted_ZeroQuotaSkipped(t *testing.T) {
	cfg := &stubCfg{rules: map[string][]configstore.ResolvedRule{
		"a":    {makeRule("a", configstore.MeterRequests, 500)},
		"b":    {makeRule("b", configstore.MeterRequests, 0)},
		"c":    {makeRule("c", configstore.MeterRequests, 500)},
	}}
	rng := rand.New(rand.NewSource(42))
	sel, _, _ := newWeightedSel(t, cfg, rng)
	ctx := context.Background()
	p := pool("w2")
	secrets := []*configstore.Secret{
		{Metadata: configstore.Metadata{Name: "a"}, KeyHash: "hA"},
		{Metadata: configstore.Metadata{Name: "b"}, KeyHash: "hB"},
		{Metadata: configstore.Metadata{Name: "c"}, KeyHash: "hC"},
	}
	for i := 0; i < 1000; i++ {
		got, err := sel.Pick(ctx, nil, p, nil, secrets)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got.Metadata.Name == "b" {
			t.Fatal("picked zero-quota secret b")
		}
	}
}

// TestPickWeighted_AllZeroReturnsErr — all secrets at zero → ErrPoolOutOfCapacity.
func TestPickWeighted_AllZeroReturnsErr(t *testing.T) {
	cfg := &stubCfg{rules: map[string][]configstore.ResolvedRule{
		"a": {makeRule("a", configstore.MeterRequests, 0)},
		"b": {makeRule("b", configstore.MeterRequests, 0)},
	}}
	rng := rand.New(rand.NewSource(42))
	sel, _, _ := newWeightedSel(t, cfg, rng)
	ctx := context.Background()
	p := pool("w3")
	secrets := []*configstore.Secret{
		{Metadata: configstore.Metadata{Name: "a"}, KeyHash: "hA"},
		{Metadata: configstore.Metadata{Name: "b"}, KeyHash: "hB"},
	}
	_, err := sel.Pick(ctx, nil, p, nil, secrets)
	if err != ErrPoolOutOfCapacity {
		t.Fatalf("want ErrPoolOutOfCapacity, got %v", err)
	}
}

// TestPickWeighted_NoLimitsFallsBackToRR — no rate limits → round-robin.
func TestPickWeighted_NoLimitsFallsBackToRR(t *testing.T) {
	cfg := &stubCfg{rules: map[string][]configstore.ResolvedRule{}}
	rng := rand.New(rand.NewSource(42))
	sel, _, _ := newWeightedSel(t, cfg, rng)
	ctx := context.Background()
	p := pool("w4")
	secrets := []*configstore.Secret{
		{Metadata: configstore.Metadata{Name: "a"}, KeyHash: "hA"},
		{Metadata: configstore.Metadata{Name: "b"}, KeyHash: "hB"},
		{Metadata: configstore.Metadata{Name: "c"}, KeyHash: "hC"},
	}
	counts := map[string]int{}
	for i := 0; i < 30; i++ {
		got, err := sel.Pick(ctx, nil, p, nil, secrets)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		counts[got.KeyHash]++
	}
	for _, k := range []string{"hA", "hB", "hC"} {
		if counts[k] < 9 || counts[k] > 11 {
			t.Fatalf("want even RR distribution, got %v", counts)
		}
	}
}

// TestPickWeighted_DistinctFromCircuitOpen — all quota zero → ErrPoolOutOfCapacity;
// all circuit open → ErrNoHealthyKeys. Two distinct sentinels.
func TestPickWeighted_DistinctFromCircuitOpen(t *testing.T) {
	ctx := context.Background()
	p := pool("w5")

	// All quota zero → ErrPoolOutOfCapacity
	cfg := &stubCfg{rules: map[string][]configstore.ResolvedRule{
		"a": {makeRule("a", configstore.MeterRequests, 0)},
	}}
	rng := rand.New(rand.NewSource(42))
	sel, _, _ := newWeightedSel(t, cfg, rng)
	secrets := []*configstore.Secret{{Metadata: configstore.Metadata{Name: "a"}, KeyHash: "hA"}}
	_, err := sel.Pick(ctx, nil, p, nil, secrets)
	if err != ErrPoolOutOfCapacity {
		t.Fatalf("want ErrPoolOutOfCapacity, got %v", err)
	}

	// All circuit open → ErrNoHealthyKeys (no limiter)
	sel2, _ := newSel(t, frozenClock(t0))
	sel2.RecordFailure(ctx, "hA", FailureAuth, 0)
	_, err2 := sel2.Pick(ctx, nil, p, nil, secrets)
	if err2 != ErrNoHealthyKeys {
		t.Fatalf("want ErrNoHealthyKeys, got %v", err2)
	}
	if err == err2 {
		t.Fatal("ErrPoolOutOfCapacity and ErrNoHealthyKeys must be distinct")
	}
}

// TestPickWeighted_DeterministicWithSeededRNG — same seed → same sequence.
func TestPickWeighted_DeterministicWithSeededRNG(t *testing.T) {
	cfg := &stubCfg{rules: map[string][]configstore.ResolvedRule{
		"a": {makeRule("a", configstore.MeterRequests, 700)},
		"b": {makeRule("b", configstore.MeterRequests, 300)},
	}}
	ctx := context.Background()
	p := pool("w6")
	secrets := []*configstore.Secret{
		{Metadata: configstore.Metadata{Name: "a"}, KeyHash: "hA"},
		{Metadata: configstore.Metadata{Name: "b"}, KeyHash: "hB"},
	}

	picks := func(seed int64) []string {
		rng := rand.New(rand.NewSource(seed))
		ms := kv.NewMem()
		defer ms.Close()
		l := limit.New(ms, noopLogger(), nil)
		sel := New(ms, noopLogger(), frozenClock(t0), l, cfg, rng)
		var out []string
		for i := 0; i < 20; i++ {
			got, err := sel.Pick(ctx, nil, p, nil, secrets)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			out = append(out, got.Metadata.Name)
		}
		return out
	}

	seq1 := picks(42)
	seq2 := picks(42)
	for i := range seq1 {
		if seq1[i] != seq2[i] {
			t.Fatalf("sequences differ at %d: %v vs %v", i, seq1, seq2)
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
