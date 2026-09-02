package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi/control"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/settings"
	pkgsecret "github.com/wyolet/relay/pkg/secret"
)

// tokenSigningKeyID is the stored-secret row the Ed25519 seed lives in.
const tokenSigningKeyID = "auth-tokens-signing-key"

// loadTokenSigningKey brings the inference-token key up: generate one on
// first boot (recording the ref back into the section so later boots reuse
// it), then hand the seed to the minting and verifying sides. Without a
// master key there is nowhere to keep the seed, so tokens stay off.
func loadTokenSigningKey(ctx context.Context, stores *appcatalog.Stores, masterKey []byte,
	signer *control.TokenSigner, verifier *inference.TokenVerifier) error {
	row, err := stores.Settings.Get(ctx, settings.AuthTokensSection)
	if err != nil {
		return fmt.Errorf("read %s: %w", settings.AuthTokensSection, err)
	}
	cfg, _ := row.Value.(*settings.AuthTokens)
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	if cfg.SigningKey.ID == "" {
		if len(masterKey) == 0 {
			slog.Warn("auth: inference tokens are disabled — no RELAY_MASTER_KEY to store a signing key under")
			return nil
		}
		seed := make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return fmt.Errorf("generate signing key: %w", err)
		}
		ref, err := stores.Stored.Create(ctx, tokenSigningKeyID, seed)
		if err != nil {
			return fmt.Errorf("store signing key: %w", err)
		}
		next := *cfg
		next.SigningKey = ref
		raw, err := json.Marshal(next)
		if err != nil {
			return fmt.Errorf("encode %s: %w", settings.AuthTokensSection, err)
		}
		if _, err := stores.Settings.Upsert(ctx, settings.AuthTokensSection, raw); err != nil {
			return fmt.Errorf("record signing key ref: %w", err)
		}
		cfg = &next
		slog.Info("auth: generated an inference-token signing key", "secret", ref.ID)
	}
	if err := applyTokenSigningKey(ctx, stores.Secrets, cfg.SigningKey, signer, verifier); err != nil {
		// Tokens are one feature; a key that no longer decrypts (rotated
		// master key, deleted secret) must not stop the gateway from serving
		// inference. Mint answers 503 until the ref is fixed.
		slog.Warn("auth: inference tokens disabled — signing key unusable",
			"err", err, "secret", cfg.SigningKey.ID)
		signer.SetSeed(nil)
		verifier.SetKey(nil)
	}
	return nil
}

// applyTokenSigningKey resolves the ref and installs the key on both sides.
// An empty ref clears them, which disables tokens without a restart.
func applyTokenSigningKey(ctx context.Context, secrets *pkgsecret.Registry, ref pkgsecret.Ref,
	signer *control.TokenSigner, verifier *inference.TokenVerifier) error {
	if ref.ID == "" {
		signer.SetSeed(nil)
		verifier.SetKey(nil)
		return nil
	}
	seed, err := secrets.Resolve(ctx, ref)
	if err != nil {
		return fmt.Errorf("resolve signing key: %w", err)
	}
	signer.SetSeed(seed)
	verifier.SetKey(signer.PublicKey())
	return nil
}
