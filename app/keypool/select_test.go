package keypool

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/pkg/kv"
)

func TestCandidate_SuccessNeedsWrite(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    Candidate
		want bool
	}{
		{"no record", Candidate{}, false},
		{"unstored record is never written", Candidate{Record: CircuitRecord{State: CircuitHalfOpen, BackoffStep: 2, Failures: 1}}, false},
		{"clean closed", Candidate{Stored: true}, false},
		{"closed with a backoff step", Candidate{Record: CircuitRecord{BackoffStep: 2}, Stored: true}, true},
		{"closed with a failure count", Candidate{Record: CircuitRecord{Failures: 1}, Stored: true}, true},
		{"half-open probe", Candidate{Record: CircuitRecord{State: CircuitHalfOpen}, Stored: true}, true},
	} {
		if got := tc.c.SuccessNeedsWrite(); got != tc.want {
			t.Errorf("%s: SuccessNeedsWrite = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The clock moves a second per pick, so least-recently-used stamps are distinct. Every case holds for a selector reading the store per pick and for one picking from a PickState.
func TestPick_Strategies(t *testing.T) {
	ctx := context.Background()
	anon := hostkey.AnonIDPrefix + "host"
	for _, tc := range []struct {
		name    string
		algo    KeySelection
		keys    []string
		open    []string
		exclude []string
		want    []string // nil: every pick is ErrNoHealthyKeys
	}{
		{name: "default is prioritized: the first key every time", keys: []string{"hA", "hB", "hC"}, want: []string{"hA", "hA", "hA"}},
		{name: "unknown strategy is prioritized", algo: "weighted", keys: []string{"hA", "hB"}, want: []string{"hA", "hA"}},
		{name: "prioritized skips an open key", algo: KeySelectionPrioritized, keys: []string{"hA", "hB", "hC"}, open: []string{"hA"}, want: []string{"hB", "hB"}},
		{name: "prioritized skips excluded keys", algo: KeySelectionPrioritized, keys: []string{"hA", "hB", "hC"}, exclude: []string{"hA", "hB"}, want: []string{"hC", "hC"}},
		{name: "prioritized takes an anonymous key unread", algo: KeySelectionPrioritized, keys: []string{anon, "hA"}, open: []string{anon}, want: []string{anon, anon}},
		{name: "round-robin takes each key in turn", algo: KeySelectionRoundRobin, keys: []string{"hA", "hB", "hC"}, want: []string{"hA", "hB", "hC", "hA", "hB", "hC", "hA"}},
		{name: "round-robin rotates over the healthy keys only", algo: KeySelectionRoundRobin, keys: []string{"hA", "hB", "hC"}, open: []string{"hB"}, want: []string{"hA", "hC", "hA", "hC"}},
		{name: "round-robin skips excluded keys", algo: KeySelectionRoundRobin, keys: []string{"hA", "hB", "hC"}, exclude: []string{"hA"}, want: []string{"hB", "hC", "hB"}},
		{name: "least-recently-used takes unused keys in order, then the oldest", algo: KeySelectionLeastRecentlyUsed, keys: []string{"hA", "hB", "hC"}, want: []string{"hA", "hB", "hC", "hA", "hB", "hC"}},
		{name: "least-recently-used skips open and excluded keys", algo: KeySelectionLeastRecentlyUsed, keys: []string{"hA", "hB", "hC", "hD"}, open: []string{"hA"}, exclude: []string{"hC"}, want: []string{"hB", "hD", "hB", "hD"}},
		{name: "prioritized with every key open", algo: KeySelectionPrioritized, keys: []string{"hA", "hB"}, open: []string{"hA", "hB"}},
		{name: "round-robin with every key open", algo: KeySelectionRoundRobin, keys: []string{"hA", "hB"}, open: []string{"hA", "hB"}},
		{name: "least-recently-used with every key open", algo: KeySelectionLeastRecentlyUsed, keys: []string{"hA", "hB"}, open: []string{"hA", "hB"}},
		{name: "every key excluded", algo: KeySelectionRoundRobin, keys: []string{"hA"}, exclude: []string{"hA"}},
		{name: "no keys", algo: KeySelectionRoundRobin},
	} {
		for _, held := range []bool{false, true} {
			name := tc.name + "/store"
			if held {
				name = tc.name + "/pick state"
			}
			t.Run(name, func(t *testing.T) {
				mem := kv.NewMem()
				t.Cleanup(func() { _ = mem.Close() })
				now := t0
				sel := New(mem, noopLogger(), func() time.Time { return now }, nil)
				if held {
					sel = newPod(mem, &now).sel
				}
				for _, h := range tc.open {
					sel.RecordFailure(ctx, h, FailureAuth, 0)
				}
				var got []string
				for range max(len(tc.want), 2) {
					now = now.Add(time.Second)
					k, err := sel.PickWithExclude(ctx, "p", tc.algo, keys(tc.keys...), keys(tc.exclude...))
					if tc.want == nil {
						if k != nil || !errors.Is(err, ErrNoHealthyKeys) {
							t.Fatalf("Pick = %v, %v; want ErrNoHealthyKeys", k, err)
						}
						continue
					}
					if err != nil {
						t.Fatalf("Pick after %v: %v", got, err)
					}
					got = append(got, k.KeyHash)
				}
				if d := cmp.Diff(tc.want, got); d != "" {
					t.Errorf("picks (-want +got):\n%s", d)
				}
			})
		}
	}
}

// Least-recently-used keeps one stamp per scope and key in the store, in unix milliseconds; a stamp it cannot read counts as never used.
func TestPick_LeastRecentlyUsedStamps(t *testing.T) {
	ctx := context.Background()
	sel, mem := newSel(t)
	ks := keys("hA", "hB")
	for i, want := range []string{"hA", "hB"} {
		at(sel, time.Duration(i)*time.Second)
		if k, err := sel.Pick(ctx, "p", KeySelectionLeastRecentlyUsed, ks); err != nil || k.KeyHash != want {
			t.Fatalf("pick %d = %v, %v; want %s", i, k, err, want)
		}
	}
	for h, want := range map[string]time.Time{"hA": t0, "hB": t0.Add(time.Second)} {
		if b, err := mem.Get(ctx, lruKey("p", h)); err != nil || string(b) != strconv.FormatInt(want.UnixMilli(), 10) {
			t.Errorf("stamp of %s = %q, %v; want %d", h, b, err, want.UnixMilli())
		}
	}
	if k, _ := sel.Pick(ctx, "other", KeySelectionLeastRecentlyUsed, ks[1:]); k != ks[1] {
		t.Errorf("pick in another scope = %v, want hB", k)
	}
	if _, err := mem.Get(ctx, lruKey("other", "hA")); !errors.Is(err, kv.ErrNotFound) {
		t.Errorf("another scope holds a stamp for a key it never picked: %v", err)
	}

	if err := mem.Set(ctx, lruKey("p", "hB"), []byte("junk"), 0); err != nil {
		t.Fatal(err)
	}
	if k, _ := sel.Pick(ctx, "p", KeySelectionLeastRecentlyUsed, ks); k != ks[1] {
		t.Errorf("pick with an unreadable stamp on hB = %v, want hB as never used", k)
	}
}

// PickCandidate reports the record the key was picked on: none for a key that never failed or the anonymous key, the claimed probe for a key whose cooldown ran out.
func TestPickCandidate_ReportsTheRecord(t *testing.T) {
	ctx := context.Background()
	sel, _ := newSel(t)
	ks := keys("hA")
	anon := keys(hostkey.AnonIDPrefix + "host")
	sel.RecordFailure(ctx, anon[0].KeyHash, FailureAuth, 0)

	for _, tc := range []struct {
		name string
		keys []*hostkey.HostKey
		prep func()
		want Candidate
	}{
		{"never failed", ks, func() {}, Candidate{Key: ks[0]}},
		{"anonymous", anon, func() {}, Candidate{Key: anon[0]}},
		{"counted failure", ks, func() { sel.RecordFailure(ctx, "hA", FailureServerError, 0) },
			Candidate{Key: ks[0], Stored: true, Record: CircuitRecord{Failures: 1, WindowStart: t0}}},
		{"probe", ks, func() {
			sel.RecordFailure(ctx, "hA", FailureRateLimitLong, 5*time.Second)
			at(sel, 5*time.Second)
		}, Candidate{Key: ks[0], Stored: true, Record: CircuitRecord{
			State: CircuitHalfOpen, OpenUntil: t0.Add(5 * time.Second), LastTransition: t0.Add(5 * time.Second),
			Reason: ReasonUpstreamRateLimited, ProbeUntil: t0.Add(5*time.Second + probeLease),
		}}},
	} {
		tc.prep()
		got, err := sel.PickCandidate(ctx, "p", KeySelectionRoundRobin, tc.keys, nil)
		if d := cmp.Diff(tc.want, got, cmp.AllowUnexported(hostkey.HostKey{})); d != "" || err != nil {
			t.Fatalf("%s: err = %v (-want +got):\n%s", tc.name, err, d)
		}
	}
	if c, err := sel.PickCandidate(ctx, "p", KeySelectionRoundRobin, ks, nil); err != ErrNoHealthyKeys || c != (Candidate{}) {
		t.Fatalf("pick while the probe is out = %+v, %v; want ErrNoHealthyKeys", c, err)
	}
}

// A record in the stored form used before times were milliseconds, written as indefinite before cooldowns were bounded, reads as an expired open record: the next pick probes the key and keeps its backoff step.
func TestPick_ProbesALegacyRecord(t *testing.T) {
	ctx := context.Background()
	sel, mem := newSel(t)
	legacy := `{"state":1,"open_until":"2026-01-01T01:00:00Z","backoff_step":2,"last_transition":"2026-01-01T00:00:00Z","indefinite":true,"reason":"upstream_auth_failed"}`
	if err := mem.Set(ctx, circuitKey("h"), []byte(legacy), 0); err != nil {
		t.Fatal(err)
	}
	if k, err := sel.Pick(ctx, "p", KeySelectionPrioritized, keys("h")); err != nil || k.KeyHash != "h" {
		t.Fatalf("pick = %v, %v; want the key as a probe", k, err)
	}
	wantRecord(t, sel, "h", &CircuitRecord{
		State: CircuitHalfOpen, BackoffStep: 2, LastTransition: t0, Reason: ReasonUpstreamAuthFailed, ProbeUntil: t0.Add(probeLease),
	})
}

func TestPickTurn(t *testing.T) {
	healthy := []Candidate{{Key: key("a", "hA")}, {Key: key("b", "hB")}, {Key: key("c", "hC")}}
	for _, tc := range []struct {
		turn int64
		n    int
		want int
	}{
		{1, 3, 0}, {2, 3, 1}, {3, 3, 2}, {4, 3, 0}, {7, 1, 0},
		{0, 3, 2}, {-1, 3, 1}, {-2, 3, 0}, {-3, 3, 2},
	} {
		if got := pickTurn(tc.turn, healthy[:tc.n]); got.Key != healthy[tc.want].Key {
			t.Errorf("pickTurn(%d) over %d keys = %s, want %s", tc.turn, tc.n, got.Key.KeyHash, healthy[tc.want].Key.KeyHash)
		}
	}
	// Without a counter value the rotation falls back to the first healthy key.
	for name, rotation := range map[string]*kv.BatchResult{"no counter": nil, "failed INCR": {Err: kv.ErrUnavailable, Int: 3}} {
		if got := pickRoundRobin(rotation, healthy); got.Key != healthy[0].Key {
			t.Errorf("pickRoundRobin with %s = %s, want hA", name, got.Key.KeyHash)
		}
	}
	if got := pickRoundRobin(&kv.BatchResult{Int: 3}, healthy); got.Key != healthy[2].Key {
		t.Errorf("pickRoundRobin at counter 3 = %s, want hC", got.Key.KeyHash)
	}
}

func TestPickOps(t *testing.T) {
	for _, tc := range []struct {
		algo KeySelection
		n    int
		want int
	}{
		{KeySelectionRoundRobin, 4, 6}, {KeySelectionRoundRobin, 0, 2},
		{KeySelectionLeastRecentlyUsed, 4, 4},
		{KeySelectionPrioritized, 4, 1}, {KeySelectionPrioritized, 0, 0}, {"", 3, 1},
	} {
		if got := PickOps(tc.algo, tc.n); got != tc.want {
			t.Errorf("PickOps(%q, %d) = %d, want %d", tc.algo, tc.n, got, tc.want)
		}
	}
}

// The index comes from the counter's atomic INCR, so concurrent picks each take a distinct turn: 30 picks over three keys land exactly ten on each. A lock that fell back under contention would pile onto the first key.
func TestPick_RoundRobinRotatesUnderContention(t *testing.T) {
	sel, _ := newSel(t)
	ks := keys("hA", "hB", "hC")
	const picks = 30
	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		mu    sync.Mutex
		count = map[string]int{}
	)
	for range picks {
		wg.Go(func() {
			<-start
			k, err := sel.Pick(context.Background(), "p", KeySelectionRoundRobin, ks)
			if err != nil {
				t.Errorf("concurrent Pick: %v", err)
				return
			}
			mu.Lock()
			count[k.KeyHash]++
			mu.Unlock()
		})
	}
	close(start)
	wg.Wait()
	if d := cmp.Diff(map[string]int{"hA": 10, "hB": 10, "hC": 10}, count); d != "" {
		t.Errorf("picks per key (-want +got):\n%s", d)
	}
}
