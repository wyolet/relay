package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi/control"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/pkg/ids"
	pkgsecret "github.com/wyolet/relay/pkg/secret"
)

// tokenSigningKeyID is the stored-secret row the Ed25519 seed lives in.
const tokenSigningKeyID = "auth-tokens-signing-key"

// tokenSigningKeyLock is the advisory-lock id generation serializes on.
// Arbitrary but fixed: every pod must pick the same number.
const tokenSigningKeyLock int64 = 0x52454C41595F544B

// noMasterKeyWarn keeps the boot path and the settings watcher's first
// delivery — both of which run this function — from logging the same
// warning twice on every start.
var noMasterKeyWarn sync.Once

// loadTokenSigningKey brings the inference-token key up: generate one on
// first boot (recording the ref back into the section so later boots reuse
// it), then hand the seed to the minting and verifying sides. Without a
// master key there is nowhere to keep the seed, so tokens stay off.
func loadTokenSigningKey(ctx context.Context, pool *pgxpool.Pool, stores *appcatalog.Stores, masterKey []byte,
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
			noMasterKeyWarn.Do(func() {
				slog.Warn("auth: inference tokens are disabled — no RELAY_MASTER_KEY to store a signing key under")
			})
			return nil
		}
		cfg, err = generateSigningKey(ctx, pool, stores, tokenSigningKeyID, false)
		if err != nil {
			return err
		}
		if cfg == nil {
			return nil
		}
	}
	verifier.SetCacheSize(cfg.EffectiveVerifyCacheSize())
	if err := applyTokenSigningKey(ctx, stores.Secrets, cfg, signer, verifier); err != nil {
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

// generateSigningKey mints a seed, stores it under secretID and records the
// ref in the auth:tokens section, returning the section as written. The
// whole read-generate-write runs under a Postgres advisory lock: two pods
// booting together would otherwise each store a key and the loser's tokens
// would verify nowhere. rotating skips the re-read shortcut — an operator
// asked for a new key even though one exists. Returns (nil, nil) when
// another pod won the race and the caller should just re-read.
func generateSigningKey(ctx context.Context, pool *pgxpool.Pool, stores *appcatalog.Stores,
	secretID string, rotating bool) (*settings.AuthTokens, error) {
	var out *settings.AuthTokens
	write := func(ctx context.Context) error {
		row, err := stores.Settings.Get(ctx, settings.AuthTokensSection)
		if err != nil {
			return fmt.Errorf("read %s: %w", settings.AuthTokensSection, err)
		}
		cfg, _ := row.Value.(*settings.AuthTokens)
		if cfg == nil {
			return fmt.Errorf("read %s: section is absent", settings.AuthTokensSection)
		}
		if !rotating && cfg.SigningKey.ID != "" {
			out = cfg
			return nil
		}
		previous := cfg.SigningKey
		seed := make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return fmt.Errorf("generate signing key: %w", err)
		}
		ref, err := stores.Stored.Create(ctx, secretID, seed)
		if err != nil {
			return fmt.Errorf("store signing key: %w", err)
		}
		next := *cfg
		next.SigningKey = ref
		// Kept so every pod (not only this one) verifies tokens minted
		// under the outgoing key.
		next.PreviousSigningKey = previous
		raw, err := json.Marshal(next)
		if err != nil {
			return fmt.Errorf("encode %s: %w", settings.AuthTokensSection, err)
		}
		if _, err := stores.Settings.Upsert(ctx, settings.AuthTokensSection, raw); err != nil {
			return fmt.Errorf("record signing key ref: %w", err)
		}
		out = &next
		slog.Info("auth: generated an inference-token signing key", "secret", ref.ID)
		return nil
	}
	if pool == nil {
		return out, write(ctx)
	}
	if err := storage.WithAdvisoryLock(ctx, pool, tokenSigningKeyLock, write); err != nil {
		return nil, err
	}
	return out, nil
}

// rotateTokenSigningKey generates a fresh key under a new secret id and
// records it. The section change is what carries the new ref to the other
// pods; this one installs it directly, keeping the outgoing public key on
// the verifier so tokens already minted keep verifying.
func rotateTokenSigningKey(ctx context.Context, pool *pgxpool.Pool, stores *appcatalog.Stores, masterKey []byte,
	signer *control.TokenSigner, verifier *inference.TokenVerifier) error {
	if len(masterKey) == 0 {
		return fmt.Errorf("no master key to store a signing key under")
	}
	cfg, err := generateSigningKey(ctx, pool, stores, tokenSigningKeyID+"-"+ids.New(), true)
	if err != nil {
		return err
	}
	return applyTokenSigningKey(ctx, stores.Secrets, cfg, signer, verifier)
}

// applyAuthTokensSection carries a live auth:tokens change onto the signer
// and the verifier. Tokens switched on after boot have no key yet, so the
// generate-and-record path runs here too — otherwise minting stays 503
// until the next restart.
func applyAuthTokensSection(ctx context.Context, pool *pgxpool.Pool, stores *appcatalog.Stores, masterKey []byte,
	a settings.AuthTokens, signer *control.TokenSigner, verifier *inference.TokenVerifier) error {
	verifier.SetCacheSize(a.EffectiveVerifyCacheSize())
	if a.Enabled && a.SigningKey.ID == "" {
		return loadTokenSigningKey(ctx, pool, stores, masterKey, signer, verifier)
	}
	if !a.Enabled {
		a.SigningKey, a.PreviousSigningKey = pkgsecret.Ref{}, pkgsecret.Ref{}
	}
	return applyTokenSigningKey(ctx, stores.Secrets, &a, signer, verifier)
}

// applyTokenSigningKey resolves the section's refs and installs the keys on
// both sides: the current one mints and verifies, the previous one only
// verifies. An empty current ref clears them, which disables tokens without
// a restart. A previous ref that no longer resolves is logged and skipped —
// minting under the live key must not depend on the retired one.
func applyTokenSigningKey(ctx context.Context, secrets *pkgsecret.Registry, cfg *settings.AuthTokens,
	signer *control.TokenSigner, verifier *inference.TokenVerifier) error {
	if cfg == nil || cfg.SigningKey.ID == "" {
		signer.SetSeed(nil)
		verifier.SetKey(nil)
		return nil
	}
	seed, err := secrets.Resolve(ctx, cfg.SigningKey)
	if err != nil {
		return fmt.Errorf("resolve signing key: %w", err)
	}
	signer.SetSeed(seed)
	verifier.SetKeys(signer.PublicKey(), previousPublicKey(ctx, secrets, cfg.PreviousSigningKey))
	return nil
}

// previousPublicKey resolves the retired signing key's public half, or nil.
func previousPublicKey(ctx context.Context, secrets *pkgsecret.Registry, ref pkgsecret.Ref) ed25519.PublicKey {
	if ref.ID == "" {
		return nil
	}
	seed, err := secrets.Resolve(ctx, ref)
	if err != nil || len(seed) != ed25519.SeedSize {
		slog.Warn("auth: previous token signing key unusable; tokens minted under it will not verify",
			"secret", ref.ID, "err", err)
		return nil
	}
	pub, _ := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	return pub
}
