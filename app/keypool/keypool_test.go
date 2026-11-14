package keypool

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/meta"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func noopLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func frozenClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func key(name, hash string) *hostkey.HostKey {
	return &hostkey.HostKey{Meta: meta.Metadata{Name: name}, KeyHash: hash}
}

func keys(hashes ...string) []*hostkey.HostKey {
	out := make([]*hostkey.HostKey, len(hashes))
	for i, h := range hashes {
		out[i] = key(h, h)
	}
	return out
}

func newSel(t *testing.T) (*Selector, *kv.Mem) {
	t.Helper()
	ms := kv.NewMem()
	t.Cleanup(func() { _ = ms.Close() })
	return New(ms, noopLogger(), frozenClock(t0), nil), ms
}

// at moves sel's clock to t0+d.
func at(sel *Selector, d time.Duration) { sel.clock = frozenClock(t0.Add(d)) }

// wantRecord asserts the stored record of keyHash; nil means the store holds none.
func wantRecord(t *testing.T, sel *Selector, keyHash string, want *CircuitRecord) {
	t.Helper()
	got, found := sel.ReadCircuit(context.Background(), keyHash)
	if want == nil {
		if found || got != (CircuitRecord{}) {
			t.Fatalf("record of %s = %+v (found %v), want none", keyHash, got, found)
		}
		return
	}
	if d := cmp.Diff(*want, got); d != "" || !found {
		t.Fatalf("record of %s (found %v) (-want +got):\n%s", keyHash, found, d)
	}
}

// noScripts hides the store's script runner, so the selector applies transitions from Go.
type noScripts struct{ kv.Store }

func TestNew_DefaultsAndBreakerOverrides(t *testing.T) {
	sel, _ := newSel(t)
	if sel.runner == nil {
		t.Fatal("a Mem-backed selector has no script runner")
	}
	if got := New(noScripts{kv.NewMem()}, noopLogger(), nil, nil); got.runner != nil || got.clock == nil || got.rng == nil {
		t.Fatalf("selector over a store without scripts = %+v, want no runner and a default clock and rng", got)
	}
	for _, tc := range []struct {
		threshold     int
		window        time.Duration
		wantThreshold int
		wantWindow    time.Duration
	}{
		{0, 0, DefaultFailureThreshold, DefaultFailureWindow},
		{-1, -time.Second, DefaultFailureThreshold, DefaultFailureWindow},
		{2, 0, 2, DefaultFailureWindow},
		{0, time.Minute, DefaultFailureThreshold, time.Minute},
		{1, time.Millisecond, 1, time.Millisecond},
	} {
		sel, _ := newSel(t)
		sel.WithBreaker(tc.threshold, tc.window)
		if sel.failureThreshold != tc.wantThreshold || sel.failureWindow != tc.wantWindow {
			t.Errorf("WithBreaker(%d, %v) = %d within %v, want %d within %v", tc.threshold, tc.window, sel.failureThreshold, sel.failureWindow, tc.wantThreshold, tc.wantWindow)
		}
	}
}
