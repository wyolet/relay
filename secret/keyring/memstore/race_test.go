package memstore_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/secret/keyring"
	"github.com/wyolet/relay/secret/keyring/memstore"
)

// TestDestroyAtomicWithWrites races rotations, writes checked by RequirePrimary and destroys; a destroyed key must never end up referenced by a row.
func TestDestroyAtomicWithWrites(t *testing.T) {
	var (
		rowsMu sync.Mutex
		rows   = map[string][16]byte{}
	)
	inUse := func(id [16]byte) bool {
		rowsMu.Lock()
		defer rowsMu.Unlock()
		for _, k := range rows {
			if k == id {
				return true
			}
		}
		return false
	}
	s := memstore.New(memstore.WithInUse(inUse))
	if err := s.InsertPrimary(ctx, primary(0, "s", t0), nil); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	run := func(fn func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				fn(i)
			}
		}()
	}
	run(func(i int) {
		cur, err := s.Primary(ctx, "s")
		if err != nil {
			t.Error(err)
			return
		}
		id := i + 1
		k := primary(0, "s", t0)
		k.ID = [16]byte{byte(id), byte(id >> 8), byte(id >> 16), 0xaa}
		if err := s.InsertPrimary(ctx, k, &cur.ID); err != nil && !errors.Is(err, keyring.ErrConflict) {
			t.Error(err)
		}
	})
	for w := 0; w < 4; w++ {
		run(func(i int) {
			// The Primary read here may be stale by the time of the write; RequirePrimary must catch that.
			cur, err := s.Primary(ctx, "s")
			if err != nil {
				t.Error(err)
				return
			}
			name := fmt.Sprint("row", (w*7+i)%5)
			err = s.RequirePrimary(cur.ID, "s", func() error {
				rowsMu.Lock()
				rows[name] = cur.ID
				rowsMu.Unlock()
				return nil
			})
			if err != nil && !errors.Is(err, keyring.ErrNotPrimary) {
				t.Error(err)
			}
		})
	}
	destroyed := 0
	run(func(int) {
		all, _ := s.List(ctx)
		for _, k := range all {
			if k.State != keyring.DecryptOnly {
				continue
			}
			switch err := s.Destroy(ctx, k.ID, 0); {
			case err == nil:
				destroyed++
				// Writes under a non-Primary key are refused for good, so this holds from here on.
				if inUse(k.ID) {
					t.Errorf("destroyed key %x is referenced by a row", k.ID)
				}
			case !errors.Is(err, keyring.ErrInUse):
				t.Error(err)
			}
		}
	})
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
	all, _ := s.List(ctx)
	state := map[[16]byte]keyring.State{}
	for _, k := range all {
		state[k.ID] = k.State
	}
	for name, id := range rows {
		if state[id] == keyring.Destroyed {
			t.Fatalf("row %s is under destroyed key %x", name, id)
		}
	}
	if destroyed == 0 {
		t.Fatal("no key was destroyed; the race was not exercised")
	}
}
