// Package oauth resolves OAuth credentials for the relay's secret layer. The
// stored secret is an OAuth token blob (access + refresh + expiry, the
// sdk/oauth.Token wire shape) held as AES-GCM ciphertext in the same
// secret_values table as KindStored. Resolve returns the access token,
// transparently refreshing via the refresh token when it has expired and
// re-persisting the (often rotated) blob.
//
// It is deliberately off the hot path: resolution happens at snapshot load and
// during KeyAgent heal after an upstream 401, never per request. Concurrent
// resolves of the same credential single-flight to one refresh.
//
// Layering: imports sdk/oauth (the flow machinery) + pkg/secret (the Ref/Vault
// contract). It never imports app/ — the provider config is injected as a plain
// lookup func by the composition root, which reads the live oauth:<provider>
// settings section.
package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/wyolet/relay/pkg/secret"
	sdkoauth "github.com/wyolet/relay/sdk/oauth"
)

// Vault is the encrypt/decrypt persistence the resolver delegates to — the
// StoredResolver in practice. The OAuth blob round-trips as a KindStored value.
type Vault interface {
	Resolve(ctx context.Context, ref secret.Ref) ([]byte, error)
	Create(ctx context.Context, id string, plaintext []byte) (secret.Ref, error)
}

// ProviderConfigFunc returns the OAuth config for a provider name (the
// oauth:<provider> settings section), or ok=false when unconfigured.
type ProviderConfigFunc func(provider string) (sdkoauth.ProviderConfig, bool)

// Resolver implements secret.Resolver for KindOAuth.
type Resolver struct {
	vault Vault
	cfg   ProviderConfigFunc
	sf    singleflight.Group
	now   func() time.Time
	skew  time.Duration
}

var _ secret.Resolver = (*Resolver)(nil)

// NewResolver builds an OAuth resolver. vault provides the AES-GCM persistence
// (a *secret.StoredResolver); cfg looks up the provider's flow config.
func NewResolver(vault Vault, cfg ProviderConfigFunc) *Resolver {
	return &Resolver{vault: vault, cfg: cfg, now: time.Now, skew: time.Minute}
}

// Resolve returns the current access token for an OAuth credential, refreshing
// first if it has expired (within a small skew). A refresh that yields a new
// token re-persists the blob so the rotated refresh token is not lost.
func (r *Resolver) Resolve(ctx context.Context, ref secret.Ref) ([]byte, error) {
	if ref.Kind != secret.KindOAuth {
		return nil, fmt.Errorf("secret/oauth: wrong kind %q", ref.Kind)
	}
	tok, err := r.load(ctx, ref.ID)
	if err != nil {
		return nil, err
	}
	if r.valid(tok) {
		return []byte(tok.AccessToken), nil
	}
	v, err, _ := r.sf.Do(ref.ID, func() (any, error) {
		// Re-load inside the flight: a concurrent refresh may have already
		// written a fresh blob while we waited for the lock.
		cur, err := r.load(ctx, ref.ID)
		if err != nil {
			return nil, err
		}
		if r.valid(cur) {
			return cur.AccessToken, nil
		}
		return r.refresh(ctx, ref, cur)
	})
	if err != nil {
		return nil, err
	}
	return []byte(v.(string)), nil
}

func (r *Resolver) load(ctx context.Context, id string) (*sdkoauth.Token, error) {
	raw, err := r.vault.Resolve(ctx, secret.Ref{Kind: secret.KindStored, ID: id})
	if err != nil {
		return nil, fmt.Errorf("secret/oauth: load %q: %w", id, err)
	}
	var tok sdkoauth.Token
	if err := json.Unmarshal(raw, &tok); err != nil {
		return nil, fmt.Errorf("secret/oauth: parse stored token %q: %w", id, err)
	}
	return &tok, nil
}

func (r *Resolver) refresh(ctx context.Context, ref secret.Ref, tok *sdkoauth.Token) (string, error) {
	cfg, ok := r.cfg(ref.Provider)
	if !ok {
		return "", fmt.Errorf("secret/oauth: no config for provider %q", ref.Provider)
	}
	if tok.RefreshToken == "" {
		return "", fmt.Errorf("secret/oauth: token %q expired with no refresh token", ref.ID)
	}
	nt, err := sdkoauth.New(cfg.OAuth2()).Refresh(ctx, tok)
	if err != nil {
		return "", fmt.Errorf("secret/oauth: refresh %q: %w", ref.ID, err)
	}
	if nt.AccessToken == "" {
		return "", fmt.Errorf("secret/oauth: refresh %q returned empty access token", ref.ID)
	}
	if nt.AccessToken != tok.AccessToken || nt.RefreshToken != tok.RefreshToken || !nt.Expiry.Equal(tok.Expiry) {
		blob, err := json.Marshal(nt)
		if err != nil {
			return "", fmt.Errorf("secret/oauth: marshal refreshed token: %w", err)
		}
		if _, err := r.vault.Create(ctx, ref.ID, blob); err != nil {
			return "", fmt.Errorf("secret/oauth: persist refreshed token %q: %w", ref.ID, err)
		}
	}
	return nt.AccessToken, nil
}

func (r *Resolver) valid(tok *sdkoauth.Token) bool {
	if tok.AccessToken == "" {
		return false
	}
	if tok.Expiry.IsZero() {
		return true
	}
	return tok.Expiry.After(r.now().Add(r.skew))
}
