package keypool

import (
	"context"
	"sync"
	"testing"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/pkg/kv"
)

// pickRoundRobin derives the index from the atomic Incr result with no lock,
// so concurrent Picks each observe a unique counter value. N concurrent Picks
// over 3 healthy keys must rotate, each key chosen exactly N/3 times — a
// contended lock falling back to idx=1 would collapse onto healthy[0].
func TestPickRoundRobin_RotatesUnderContention(t *testing.T) {
	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	sel := New(mem, noopLogger(), frozenClock(t0), nil)
	ctx := context.Background()
	keys := []*hostkey.HostKey{
		key("a", "hA"),
		key("b", "hB"),
		key("c", "hC"),
	}
	p := poolWithStrategy("rr-contended", KeySelectionRoundRobin)

	const picks = 30 // multiple of len(keys) → exactly even split
	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		mu    sync.Mutex
		count = map[string]int{}
	)
	for i := 0; i < picks; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			k, err := sel.Pick(ctx, p.scope, p.algo, keys)
			if err != nil {
				t.Errorf("concurrent Pick returned error: %v", err)
				return
			}
			mu.Lock()
			count[k.KeyHash]++
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	if len(count) != len(keys) {
		t.Fatalf("round-robin used %d/%d healthy keys under contention: %v (collapse regression, audit P1 #9)",
			len(count), len(keys), count)
	}
	want := picks / len(keys)
	for _, k := range keys {
		if got := count[k.KeyHash]; got != want {
			t.Errorf("key %q picked %d times, want exactly %d (atomic counter yields unique consecutive indices): %v",
				k.KeyHash, got, want, count)
		}
	}
}
