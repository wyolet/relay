package kek_test

import (
	"bytes"
	"testing"

	"github.com/wyolet/relay/secret/kek"
	"github.com/wyolet/relay/secret/kek/local"
)

func newLocal(t *testing.T, b byte) *local.KEK {
	t.Helper()
	k, err := local.New(bytes.Repeat([]byte{b}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestNewSet(t *testing.T) {
	a, b := newLocal(t, 1), newLocal(t, 2)
	s, err := kek.NewSet(a, b)
	if err != nil {
		t.Fatal(err)
	}
	ids := s.IDs()
	if len(ids) != 2 || ids[0] != a.ID() || ids[1] != b.ID() {
		t.Fatalf("IDs = %x", ids)
	}
	ids[0] = [16]byte{}
	if s.IDs()[0] != a.ID() {
		t.Fatal("IDs exposes internal slice")
	}
	if got, ok := s.Get(b.ID()); !ok || got != kek.KEK(b) {
		t.Fatal("Get(b) failed")
	}
	if _, ok := s.Get([16]byte{9}); ok {
		t.Fatal("Get(unknown) ok")
	}
}

func TestNewSetRefuses(t *testing.T) {
	a := newLocal(t, 1)
	if _, err := kek.NewSet(); err == nil {
		t.Fatal("empty set accepted")
	}
	if _, err := kek.NewSet(a, newLocal(t, 1)); err == nil {
		t.Fatal("duplicate id accepted")
	}
	if _, err := kek.NewSet(a, nil); err == nil {
		t.Fatal("nil KEK accepted")
	}
}
