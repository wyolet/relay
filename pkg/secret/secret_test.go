package secret

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/pkg/crypto"
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

func TestRefValidate(t *testing.T) {
	bad := []Ref{
		{Kind: KindEnv},          // missing env
		{Kind: KindStored},       // missing id
		{Kind: "vault", ID: "x"}, // unknown kind
		{Kind: KindEnv, ID: "x"}, // env without env name
	}
	for _, r := range bad {
		if err := r.Validate(); err == nil {
			t.Errorf("Validate(%+v): want error", r)
		}
	}
	good := []Ref{{Kind: KindEnv, Env: "FOO"}, {Kind: KindStored, ID: "abc"}}
	for _, r := range good {
		if err := r.Validate(); err != nil {
			t.Errorf("Validate(%+v): %v", r, err)
		}
	}
}

func TestEnvResolver(t *testing.T) {
	t.Setenv("MY_SECRET", "s3kr3t")
	reg := NewRegistry()
	reg.Register(KindEnv, EnvResolver{})

	got, err := reg.Resolve(context.Background(), Ref{Kind: KindEnv, Env: "MY_SECRET"})
	if err != nil || string(got) != "s3kr3t" {
		t.Fatalf("env resolve: got %q err %v", got, err)
	}
	// Unset → loud error.
	if _, err := reg.Resolve(context.Background(), Ref{Kind: KindEnv, Env: "NOPE_UNSET"}); err == nil {
		t.Fatal("unset env should error")
	}
}

func TestStoredResolver_RoundTrip(t *testing.T) {
	store := newMemStore()
	sr := NewStoredResolver(store, mustKey(t), 1)
	reg := NewRegistry()
	reg.Register(KindStored, sr)

	ref, err := sr.Create(context.Background(), "sk-1", []byte("upstream-key"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if ref.Kind != KindStored || ref.ID != "sk-1" {
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

func TestRegistry_UnknownKind(t *testing.T) {
	reg := NewRegistry()
	reg.Register(KindEnv, EnvResolver{})
	// stored not registered → clear error (e.g. minimal build).
	if _, err := reg.Resolve(context.Background(), Ref{Kind: KindStored, ID: "x"}); err == nil {
		t.Fatal("want error for unregistered kind")
	}
}
