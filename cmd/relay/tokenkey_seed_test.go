package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/wyolet/relay/app/httpapi/control"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/settings"
	pkgsecret "github.com/wyolet/relay/pkg/secret"
)

// failingResolver answers from seeds and fails for the ids in broken.
type failingResolver struct {
	seeds  map[string][]byte
	broken map[string]bool
}

func (f failingResolver) Resolve(_ context.Context, ref pkgsecret.Ref) ([]byte, error) {
	if f.broken[ref.ID] {
		return nil, errors.New("failingResolver: backend unavailable")
	}
	return f.seeds[ref.ID], nil
}

func tokenSection() *settings.AuthTokens {
	return &settings.AuthTokens{
		Enabled: true, DefaultTTL: time.Hour, MaxTTL: 24 * time.Hour,
		SigningKey:         pkgsecret.Ref{Kind: pkgsecret.KindStored, ID: "current"},
		PreviousSigningKey: pkgsecret.Ref{Kind: pkgsecret.KindStored, ID: "previous"},
	}
}

func registryFor(r pkgsecret.Resolver) *pkgsecret.Registry {
	reg := pkgsecret.NewRegistry()
	reg.Register(pkgsecret.KindStored, r)
	return reg
}

// A seed of the wrong length leaves the signer empty, so minting answers
// 503 with nothing in the log saying why.
func TestSigningSeedOfTheWrongLengthWarns(t *testing.T) {
	h := &countingHandler{needle: "not an Ed25519 seed"}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	secrets := registryFor(failingResolver{seeds: map[string][]byte{"current": []byte("short")}})
	if err := applyTokenSigningKey(context.Background(), secrets, tokenSection(),
		&control.TokenSigner{}, &inference.TokenVerifier{}); err != nil {
		t.Fatalf("applyTokenSigningKey: %v", err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.n != 1 {
		t.Fatalf("warnings = %d, want 1", h.n)
	}
}

// A previous key that fails to resolve once must be retried, not recorded
// as "there is no previous key" — which would log out every bearer of a
// token minted before the rotation.
func TestTransientPreviousKeyFailureKeepsTheKeySet(t *testing.T) {
	current, previous := make([]byte, ed25519.SeedSize), make([]byte, ed25519.SeedSize)
	current[0], previous[0] = 1, 2
	seeds := map[string][]byte{"current": current, "previous": previous}
	want, _ := ed25519.NewKeyFromSeed(previous).Public().(ed25519.PublicKey)

	signer, verifier := &control.TokenSigner{}, &inference.TokenVerifier{}
	ctx := context.Background()
	if err := applyTokenSigningKey(ctx, registryFor(failingResolver{seeds: seeds}),
		tokenSection(), signer, verifier); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	broken := registryFor(failingResolver{seeds: seeds, broken: map[string]bool{"previous": true}})
	if err := applyTokenSigningKey(ctx, broken, tokenSection(), signer, verifier); err != nil {
		t.Fatalf("apply with an unresolvable previous key: %v", err)
	}
	if got := verifier.PreviousPublicKey(); !got.Equal(want) {
		t.Fatalf("previous key = %v, want the one already installed", got)
	}
}
