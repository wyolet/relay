package keypool

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/pkg/kv"
)

// benchPickRoundRobin is one round-robin pick over four keys that never failed, so every breaker read misses.
func benchPickRoundRobin(b *testing.B, store kv.Store) {
	sel := New(store, noopLogger(), frozenClock(t0), nil)
	ks := []*hostkey.HostKey{key("a", "hA"), key("b", "hB"), key("c", "hC"), key("d", "hD")}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := sel.PickCandidate(ctx, "bench", KeySelectionRoundRobin, ks, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPickRoundRobin(b *testing.B) {
	s := kv.NewMem()
	b.Cleanup(func() { _ = s.Close() })
	benchPickRoundRobin(b, s)
}

// BenchmarkPickRoundRobinWarm is the same pick from a warm PickState: no store call.
func BenchmarkPickRoundRobinWarm(b *testing.B) {
	s := kv.NewMem()
	b.Cleanup(func() { _ = s.Close() })
	sel := New(s, noopLogger(), frozenClock(t0), nil).WithPickState(NewPickState(s, 0, noopLogger()).WithClock(frozenClock(t0)))
	ks := []*hostkey.HostKey{key("a", "hA"), key("b", "hB"), key("c", "hC"), key("d", "hD")}
	ctx := context.Background()
	if _, err := sel.PickCandidate(ctx, "bench", KeySelectionRoundRobin, ks, nil); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := sel.PickCandidate(ctx, "bench", KeySelectionRoundRobin, ks, nil); err != nil {
			b.Fatal(err)
		}
	}
}
