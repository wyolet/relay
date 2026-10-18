package memstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wyolet/relay/secret/keyring"
	"github.com/wyolet/relay/secret/keyring/memstore"
)

var ctx = context.Background()

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func primary(id byte, scope string, at time.Time) keyring.DataKey {
	return keyring.DataKey{ID: [16]byte{id}, Scope: scope, State: keyring.Primary, KEKID: [16]byte{0xee}, Wrapped: []byte{id}, CreatedAt: at}
}

func mustGet(t *testing.T, s *memstore.Store, id [16]byte) keyring.DataKey {
	t.Helper()
	k, err := s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestInsertPrimaryCAS(t *testing.T) {
	s := memstore.New()
	a, b := primary(1, "s", t0), primary(2, "s", t0.Add(time.Hour))
	if err := s.InsertPrimary(ctx, a, &a.ID); !errors.Is(err, keyring.ErrConflict) {
		t.Fatalf("expected Primary on an empty scope: %v", err)
	}
	if err := s.InsertPrimary(ctx, a, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertPrimary(ctx, b, nil); !errors.Is(err, keyring.ErrConflict) {
		t.Fatalf("nil expected over an existing Primary: %v", err)
	}
	if err := s.InsertPrimary(ctx, b, &[16]byte{9}); !errors.Is(err, keyring.ErrConflict) {
		t.Fatalf("wrong expected: %v", err)
	}
	if err := s.InsertPrimary(ctx, b, &a.ID); err != nil {
		t.Fatal(err)
	}
	if k := mustGet(t, s, a.ID); k.State != keyring.DecryptOnly || !k.DemotedAt.Equal(b.CreatedAt) {
		t.Fatalf("demoted key = %+v", k)
	}
	if p, err := s.Primary(ctx, "s"); err != nil || p.ID != b.ID {
		t.Fatalf("Primary = %+v, %v", p, err)
	}
	// Scopes are independent, "" included.
	if err := s.InsertPrimary(ctx, primary(3, "", t0), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Primary(ctx, "none"); !errors.Is(err, keyring.ErrUnknownKey) {
		t.Fatalf("Primary of an empty scope: %v", err)
	}

	dup := primary(2, "t", t0)
	if err := s.InsertPrimary(ctx, dup, nil); err == nil {
		t.Fatal("duplicate id accepted")
	}
	notPrimary := primary(4, "t", t0)
	notPrimary.State = keyring.DecryptOnly
	if err := s.InsertPrimary(ctx, notPrimary, nil); err == nil {
		t.Fatal("non-Primary insert accepted")
	}
	unwrapped := primary(5, "t", t0)
	unwrapped.Wrapped = nil
	if err := s.InsertPrimary(ctx, unwrapped, nil); err == nil {
		t.Fatal("insert without wrapped bytes accepted")
	}
}

func TestSetTargetKEKCAS(t *testing.T) {
	s := memstore.New()
	a, b := [16]byte{1}, [16]byte{2}
	if got, _ := s.TargetKEK(ctx); got != ([16]byte{}) {
		t.Fatalf("initial target = %x", got)
	}
	if err := s.SetTargetKEK(ctx, a, b); !errors.Is(err, keyring.ErrConflict) {
		t.Fatalf("wrong expected: %v", err)
	}
	if err := s.SetTargetKEK(ctx, a, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTargetKEK(ctx, b, [16]byte{}); !errors.Is(err, keyring.ErrConflict) {
		t.Fatalf("second first-set: %v", err)
	}
	if err := s.SetTargetKEK(ctx, b, a); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.TargetKEK(ctx); got != b {
		t.Fatalf("target = %x", got)
	}
	if err := s.SetTargetKEK(ctx, [16]byte{}, b); err == nil {
		t.Fatal("zero target accepted")
	}
}

func TestUpdateWrappedCAS(t *testing.T) {
	s := memstore.New()
	from, to := [16]byte{0xee}, [16]byte{0xdd}
	a := primary(1, "s", t0)
	if err := s.UpdateWrapped(ctx, a.ID, from, to, []byte("w")); !errors.Is(err, keyring.ErrUnknownKey) {
		t.Fatalf("unknown id: %v", err)
	}
	if err := s.InsertPrimary(ctx, a, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateWrapped(ctx, a.ID, to, from, []byte("w")); !errors.Is(err, keyring.ErrConflict) {
		t.Fatalf("wrong fromKEK: %v", err)
	}
	w := []byte("rewrapped")
	if err := s.UpdateWrapped(ctx, a.ID, from, to, w); err != nil {
		t.Fatal(err)
	}
	w[0] = 'X'
	if k := mustGet(t, s, a.ID); k.KEKID != to || string(k.Wrapped) != "rewrapped" || k.State != keyring.Primary {
		t.Fatalf("after rewrap = %+v", k)
	}
	if err := s.UpdateWrapped(ctx, a.ID, from, to, []byte("again")); !errors.Is(err, keyring.ErrConflict) {
		t.Fatalf("stale fromKEK: %v", err)
	}
	b := primary(2, "s", t0)
	if err := s.InsertPrimary(ctx, b, &a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Destroy(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateWrapped(ctx, a.ID, to, from, []byte("w")); !errors.Is(err, keyring.ErrConflict) {
		t.Fatalf("destroyed key: %v", err)
	}
}

func TestDestroy(t *testing.T) {
	inUse := map[[16]byte]bool{}
	s := memstore.New(memstore.WithInUse(func(id [16]byte) bool { return inUse[id] }))
	a, b := primary(1, "s", t0), primary(2, "s", t0)
	if err := s.Destroy(ctx, a.ID); !errors.Is(err, keyring.ErrUnknownKey) {
		t.Fatalf("unknown: %v", err)
	}
	if err := s.InsertPrimary(ctx, a, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Destroy(ctx, a.ID); !errors.Is(err, keyring.ErrIsPrimary) {
		t.Fatalf("Primary: %v", err)
	}
	if err := s.InsertPrimary(ctx, b, &a.ID); err != nil {
		t.Fatal(err)
	}
	inUse[a.ID] = true
	if err := s.Destroy(ctx, a.ID); !errors.Is(err, keyring.ErrInUse) {
		t.Fatalf("in use: %v", err)
	}
	inUse[a.ID] = false
	if err := s.Destroy(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if k := mustGet(t, s, a.ID); k.State != keyring.Destroyed || k.Wrapped != nil || k.Scope != "s" || k.KEKID != a.KEKID {
		t.Fatalf("tombstone = %+v", k)
	}
	if err := s.Destroy(ctx, a.ID); err != nil {
		t.Fatalf("destroy again: %v", err)
	}
}

func TestRequirePrimary(t *testing.T) {
	s := memstore.New()
	a, b := primary(1, "s", t0), primary(2, "s", t0)
	if err := s.InsertPrimary(ctx, a, nil); err != nil {
		t.Fatal(err)
	}
	calls := 0
	write := func() error { calls++; return nil }
	if err := s.RequirePrimary(a.ID, "s", write); err != nil || calls != 1 {
		t.Fatalf("Primary write: %v, %d calls", err, calls)
	}
	boom := errors.New("boom")
	if err := s.RequirePrimary(a.ID, "s", func() error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("write error: %v", err)
	}
	for name, tc := range map[string]struct {
		id    [16]byte
		scope string
	}{"other scope": {a.ID, "t"}, "unknown": {[16]byte{9}, "s"}} {
		if err := s.RequirePrimary(tc.id, tc.scope, write); !errors.Is(err, keyring.ErrNotPrimary) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if err := s.InsertPrimary(ctx, b, &a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RequirePrimary(a.ID, "s", write); !errors.Is(err, keyring.ErrNotPrimary) {
		t.Fatalf("demoted: %v", err)
	}
	if calls != 1 {
		t.Fatalf("refused writes ran: %d calls", calls)
	}
}

func TestReturnsCopies(t *testing.T) {
	s := memstore.New()
	a := primary(1, "s", t0)
	if err := s.InsertPrimary(ctx, a, nil); err != nil {
		t.Fatal(err)
	}
	a.Wrapped[0] = 0xff
	got := mustGet(t, s, a.ID)
	got.Wrapped[0] = 0xfe
	all, _ := s.List(ctx)
	all[0].Wrapped[0] = 0xfd
	p, _ := s.Primary(ctx, "s")
	p.Wrapped[0] = 0xfc
	if k := mustGet(t, s, a.ID); k.Wrapped[0] != 1 {
		t.Fatalf("stored wrapped bytes changed: %x", k.Wrapped)
	}
}
