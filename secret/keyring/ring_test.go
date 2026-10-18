package keyring_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/secret/aead"
	"github.com/wyolet/relay/secret/keyring"
	"github.com/wyolet/relay/secret/keyring/memstore"
)

func TestUnknownKeyReloadAndNegativeCache(t *testing.T) {
	c := newClock()
	ms := memstore.New()
	k1 := newKEK(t, 1)
	cs := &counting{Store: ms}
	r := newRing(t, cs, c, k1)

	// A key created elsewhere is read once on first sight.
	writer := newRing(t, ms, c, k1)
	sealed, _ := mustEncrypt(t, writer, "s", "v", "")
	mustDecrypt(t, r, "s", sealed, "", "v")
	mustDecrypt(t, r, "s", sealed, "", "v")
	if n := cs.gets.Load(); n != 1 {
		t.Fatalf("store reads for a new key = %d, want 1", n)
	}

	bogusID := [16]byte{0xde, 0xad}
	bogus, err := aead.Seal(make([]byte, 32), bogusID, []byte("x"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := r.Decrypt(ctx, "s", bogus, nil); !errors.Is(err, keyring.ErrUnknownKey) {
				t.Errorf("err = %v, want ErrUnknownKey", err)
			}
		}()
	}
	wg.Wait()
	if n := cs.gets.Load(); n != 2 {
		t.Fatalf("store reads after a burst of one unknown id = %d, want 2", n)
	}
	c.Advance(4 * time.Second)
	_, _, _ = r.Decrypt(ctx, "s", bogus, nil)
	if n := cs.gets.Load(); n != 2 {
		t.Fatalf("store reads within the TTL = %d, want 2", n)
	}
	c.Advance(2 * time.Second)
	_, _, _ = r.Decrypt(ctx, "s", bogus, nil)
	if n := cs.gets.Load(); n != 3 {
		t.Fatalf("store reads after the TTL = %d, want 3", n)
	}
	// An explicit Reload bypasses the cache.
	if err := r.Reload(ctx, bogusID); err != nil {
		t.Fatal(err)
	}
	if n := cs.gets.Load(); n != 4 {
		t.Fatalf("store reads after Reload = %d, want 4", n)
	}
	if _, _, err := r.Decrypt(ctx, "s", []byte("plaintext"), nil); !errors.Is(err, aead.ErrNotSealed) {
		t.Fatalf("plaintext: err = %v", err)
	}
}

func TestLockedKeyReloadsOnDecrypt(t *testing.T) {
	c := newClock()
	ms := memstore.New()
	k1, k2 := newKEK(t, 1), newKEK(t, 2)
	b := newRing(t, ms, c, k1, k2)
	sealed, id := mustEncrypt(t, b, "s", "v", "row")
	cs := &counting{Store: ms}
	a := newRing(t, cs, c, k2) // loads K while it is still under k1
	if info, _ := keyInfo(a, id); !info.Locked {
		t.Fatal("K not locked on a k2-only ring")
	}

	// A still-locked key is read once, then remembered for the TTL.
	for i := 0; i < 2; i++ {
		if _, _, err := a.Decrypt(ctx, "s", sealed, []byte("row")); !errors.Is(err, keyring.ErrKEKUnavailable) {
			t.Fatalf("decrypt %d under locked K: %v", i, err)
		}
	}
	if n := cs.gets.Load(); n != 1 {
		t.Fatalf("store reads for a still-locked key = %d, want 1", n)
	}

	if _, err := b.Rewrap(ctx, k2.ID()); err != nil { // a is not told
		t.Fatal(err)
	}
	c.Advance(5 * time.Second)
	if st := mustDecrypt(t, a, "s", sealed, "row", "v"); st.Stale {
		t.Fatal("value under the Primary reported stale")
	}
	mustDecrypt(t, a, "s", sealed, "row", "v")
	if n := cs.gets.Load(); n != 2 {
		t.Fatalf("store reads = %d, want 2 (one per locked miss, none once unwrapped)", n)
	}
	if info, _ := keyInfo(a, id); info.Locked || info.KEKID != k2.ID() {
		t.Fatalf("K on a after the reload = %+v", info)
	}
}

func TestReloadSeesRotateAndTarget(t *testing.T) {
	c := newClock()
	ms := memstore.New()
	k1, k2 := newKEK(t, 1), newKEK(t, 2)
	a, b := newRing(t, ms, c, k1, k2), newRing(t, ms, c, k1, k2)
	old, oldID := mustEncrypt(t, a, "s", "v", "")
	mustDecrypt(t, b, "s", old, "", "v")
	nk, err := a.Rotate(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	// Notified of the new key only, b still learns the old one was demoted.
	if err := b.Reload(ctx, nk.ID); err != nil {
		t.Fatal(err)
	}
	if info, _ := keyInfo(b, oldID); info.State != keyring.DecryptOnly {
		t.Fatalf("old key on b = %+v", info)
	}
	if st := mustDecrypt(t, b, "s", old, "", "v"); !st.Stale {
		t.Fatal("old value not stale on b")
	}
	if err := ms.SetTargetKEK(ctx, k2.ID(), k1.ID()); err != nil {
		t.Fatal(err)
	}
	if err := b.ReloadTarget(ctx); err != nil {
		t.Fatal(err)
	}
	if b.Target() != k2.ID() {
		t.Fatal("ReloadTarget did not pick up the target")
	}
}

func TestRun(t *testing.T) {
	c := newClock()
	ms := memstore.New()
	k1 := newKEK(t, 1)
	a := newRing(t, ms, c, k1)
	_, oldID := mustEncrypt(t, a, "s", "v", "")
	cs := &counting{Store: ms}
	errs := make(chan error, 16)
	b, err := keyring.New(ctx, cs, newSet(t, k1), keyring.Options{Now: c.Now, RefreshInterval: 5 * time.Millisecond, ReloadError: func(err error) { errs <- err }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Rotate(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- b.Run(runCtx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if info, _ := keyInfo(b, oldID); info.State == keyring.DecryptOnly {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run never reloaded")
		}
		time.Sleep(time.Millisecond)
	}
	boom := errors.New("store down")
	cs.listErr.Store(&boom)
	select {
	case err := <-errs:
		if !errors.Is(err, boom) {
			t.Fatalf("ReloadError got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReloadError never called")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
}

func TestNew(t *testing.T) {
	set := newSet(t, newKEK(t, 1))
	if _, err := keyring.New(ctx, nil, set, keyring.Options{}); err == nil {
		t.Fatal("nil store accepted")
	}
	if _, err := keyring.New(ctx, memstore.New(), nil, keyring.Options{}); err == nil {
		t.Fatal("nil set accepted")
	}
	if _, err := keyring.New(ctx, memstore.New(), set, keyring.Options{DestroyAfter: -time.Second}); err == nil {
		t.Fatal("negative duration accepted")
	}
	boom := errors.New("down")
	cs := &counting{Store: memstore.New()}
	cs.listErr.Store(&boom)
	if _, err := keyring.New(ctx, cs, set, keyring.Options{}); !errors.Is(err, boom) {
		t.Fatalf("store error: %v", err)
	}
}

func TestFormatID(t *testing.T) {
	id := [16]byte{0x01, 0x92, 0xf3, 0xa0, 0x12, 0x34, 0x70, 0x00, 0x80, 0x00, 0, 0, 0, 0, 0, 0xaa}
	if got := keyring.FormatID(id); got != "0192f3a0-1234-7000-8000-0000000000aa" {
		t.Fatalf("FormatID = %s", got)
	}
}

func TestConcurrentUse(t *testing.T) {
	c := newClock()
	ms := memstore.New()
	k1, k2 := newKEK(t, 1), newKEK(t, 2)
	a, b := newRing(t, ms, c, k1, k2), newRing(t, ms, c, k1, k2)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w, r := a, b
			if i%2 == 1 {
				w, r = b, a
			}
			scope := fmt.Sprint("s", i%3)
			for j := 0; j < 50; j++ {
				plain := fmt.Sprint(i, "-", j)
				sealed, _, err := w.Encrypt(ctx, scope, []byte(plain), nil)
				if err != nil {
					t.Error(err)
					return
				}
				got, _, err := r.Decrypt(ctx, scope, sealed, nil)
				if err != nil || string(got) != plain {
					t.Errorf("Decrypt = %q, %v", got, err)
					return
				}
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; ; j++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := a.Rotate(ctx, fmt.Sprint("s", j%3)); err != nil && !errors.Is(err, keyring.ErrConflict) {
				t.Error(err)
			}
			if err := b.ReloadAll(ctx); err != nil {
				t.Error(err)
			}
			_ = a.Keys()
		}
	}()
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(stop)
	}()
	wg.Wait()
}

// flightSetup returns a ring on a gated store that has not seen a key another ring just created, and a value under that key.
func flightSetup(t *testing.T, panics bool) (*keyring.Ring, *gated, []byte) {
	t.Helper()
	c := newClock()
	ms := memstore.New()
	k1 := newKEK(t, 1)
	g := newGated(ms, panics)
	r := newRing(t, g, c, k1)
	sealed, _ := mustEncrypt(t, newRing(t, ms, c, k1), "s", "v", "")
	g.armed.Store(true)
	return r, g, sealed
}

// joinFlight starts a Decrypt with a live context once the leader's read is under way and gives it time to join the flight.
func joinFlight(r *keyring.Ring, g *gated, sealed []byte) chan error {
	<-g.entered
	waiter := make(chan error, 1)
	go func() {
		_, _, err := r.Decrypt(ctx, "s", sealed, nil)
		waiter <- err
	}()
	time.Sleep(20 * time.Millisecond)
	return waiter
}

func TestFlightOutlivesCancelledLeader(t *testing.T) {
	r, g, sealed := flightSetup(t, false)
	leaderCtx, cancel := context.WithCancel(ctx)
	leader := make(chan error, 1)
	go func() {
		_, _, err := r.Decrypt(leaderCtx, "s", sealed, nil)
		leader <- err
	}()
	waiter := joinFlight(r, g, sealed)
	cancel()
	time.Sleep(20 * time.Millisecond)
	close(g.release)
	select {
	case err := <-waiter:
		if err != nil {
			t.Fatalf("waiter: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter hung")
	}
	<-leader
}

func TestFlightReleasedOnPanic(t *testing.T) {
	r, g, sealed := flightSetup(t, true)
	leader := make(chan any, 1)
	go func() {
		defer func() { leader <- recover() }()
		_, _, _ = r.Decrypt(ctx, "s", sealed, nil)
	}()
	waiter := joinFlight(r, g, sealed)
	close(g.release)
	if p := <-leader; p == nil {
		t.Fatal("leader did not panic")
	}
	select {
	case err := <-waiter:
		if err == nil {
			t.Fatal("waiter got no error from a panicked read")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter hung")
	}
	mustDecrypt(t, r, "s", sealed, "", "v")
}
