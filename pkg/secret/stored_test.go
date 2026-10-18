package secret

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/pkg/crypto"
	"github.com/wyolet/relay/secret"
)

// memStore is an in-memory secret.Store for tests.
type memStore struct {
	ct, nonce map[string][]byte
	ver       map[string]int32
}

func newMemStore() *memStore {
	return &memStore{ct: map[string][]byte{}, nonce: map[string][]byte{}, ver: map[string]int32{}}
}

func (m *memStore) Get(_ context.Context, id string) ([]byte, []byte, int32, error) {
	ct, ok := m.ct[id]
	if !ok {
		return nil, nil, 0, errors.New("not found")
	}
	return ct, m.nonce[id], m.ver[id], nil
}

func (m *memStore) Put(_ context.Context, id string, ct, nonce []byte, ver int32) error {
	m.ct[id] = ct
	m.nonce[id] = nonce
	m.ver[id] = ver
	return nil
}

func (m *memStore) Delete(_ context.Context, id string) error {
	delete(m.ct, id)
	delete(m.nonce, id)
	delete(m.ver, id)
	return nil
}

func mustKey(t *testing.T) []byte {
	t.Helper()
	raw, err := crypto.GenerateMasterKey()
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	mk, err := crypto.ParseMasterKey(raw)
	if err != nil {
		t.Fatalf("parsekey: %v", err)
	}
	return mk
}

func TestStoredResolver_RoundTrip(t *testing.T) {
	store := newMemStore()
	sr := NewStoredResolver(store, mustKey(t), 1)
	reg := secret.NewRegistry()
	reg.Register(secret.KindStored, sr)

	ref, err := sr.Create(context.Background(), "sk-1", []byte("upstream-key"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if ref.Kind != secret.KindStored || ref.ID != "sk-1" {
		t.Fatalf("ref: %+v", ref)
	}
	// Ciphertext at rest must not equal plaintext.
	if string(store.ct["sk-1"]) == "upstream-key" {
		t.Fatal("stored ciphertext equals plaintext")
	}
	got, err := reg.Resolve(context.Background(), ref)
	if err != nil || string(got) != "upstream-key" {
		t.Fatalf("resolve: got %q err %v", got, err)
	}
}
