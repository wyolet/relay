package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi/control"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/pkg/crypto"
	pkgsecret "github.com/wyolet/relay/pkg/secret"
)

// seedResolver serves fixed seeds by secret id.
type seedResolver map[string][]byte

func (m seedResolver) Resolve(_ context.Context, ref pkgsecret.Ref) ([]byte, error) {
	if seed, ok := m[ref.ID]; ok {
		return seed, nil
	}
	return nil, fmt.Errorf("seedResolver: no secret %q", ref.ID)
}

// A rotation happens on one pod; every other pod only sees the section
// change. TestBootLoadsThePreviousSigningKey pins that the retired key is
// persisted and reloaded, so a pod that never held it still verifies tokens
// minted under it instead of logging their bearers out.
func TestBootLoadsThePreviousSigningKey(t *testing.T) {
	current, previous := make([]byte, ed25519.SeedSize), make([]byte, ed25519.SeedSize)
	current[0], previous[0] = 1, 2

	cfg := settings.AuthTokens{
		Enabled: true, DefaultTTL: time.Hour, MaxTTL: 24 * time.Hour,
		SigningKey:         pkgsecret.Ref{Kind: pkgsecret.KindStored, ID: "current"},
		PreviousSigningKey: pkgsecret.Ref{Kind: pkgsecret.KindStored, ID: "previous"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("section with both refs is invalid: %v", err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal cfg: %v", err)
	}

	secrets := pkgsecret.NewRegistry()
	secrets.Register(pkgsecret.KindStored, seedResolver{"current": current, "previous": previous})
	stores := &appcatalog.Stores{
		Settings: settings.NewStore(gen.New(stubDBTX{row: settingsRow{section: settings.AuthTokensSection, value: raw}})),
		Secrets:  secrets,
	}

	signer := &control.TokenSigner{}
	verifier := &inference.TokenVerifier{}
	if err := loadTokenSigningKey(context.Background(), nil, stores, nil, signer, verifier); err != nil {
		t.Fatalf("loadTokenSigningKey: %v", err)
	}

	// A token minted under the retired key still verifies on this pod.
	retired := ed25519.NewKeyFromSeed(previous)
	claims := crypto.TokenClaims{Sub: "user:u-1", Exp: time.Now().Add(time.Hour).Unix()}
	retiredPub, _ := retired.Public().(ed25519.PublicKey)
	token, err := crypto.SignToken(retired, crypto.KeyID(retiredPub), claims)
	if err != nil {
		t.Fatalf("sign with the retired key: %v", err)
	}
	if _, err := verifier.Verify(token); err != nil {
		t.Fatalf("verify a token minted under the previous key: %v", err)
	}

	// And so does one minted under the live key.
	livePriv := ed25519.NewKeyFromSeed(current)
	livePub, _ := livePriv.Public().(ed25519.PublicKey)
	live, err := crypto.SignToken(livePriv, crypto.KeyID(livePub), claims)
	if err != nil {
		t.Fatalf("sign with the live key: %v", err)
	}
	if _, err := verifier.Verify(live); err != nil {
		t.Fatalf("verify a token minted under the current key: %v", err)
	}
}
