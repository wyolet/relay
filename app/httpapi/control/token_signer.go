package control

import (
	"context"
	"crypto/ed25519"
	"errors"
	"sync/atomic"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/pkg/crypto"
)

// TokenSigner holds the Ed25519 signing key. The composition root swaps the
// key in when the auth:tokens section changes; an empty signer refuses to
// mint, which is the posture of a deployment with no master key.
type TokenSigner struct {
	key atomic.Pointer[signingKey]
	// prev is the verification half of the key a rotation retired: minting
	// never uses it, but a token presented for revocation may carry it.
	prev atomic.Pointer[ed25519.PublicKey]
}

// signingKey pairs the private half with the `kid` the tokens it signs
// carry, so the two can never be read out of step.
type signingKey struct {
	priv ed25519.PrivateKey
	kid  string
}

// SetSeed installs the signing key from its 32-byte seed. A nil seed
// disables minting.
func (s *TokenSigner) SetSeed(seed []byte) {
	s.prev.Store(nil)
	if len(seed) != ed25519.SeedSize {
		s.key.Store(nil)
		return
	}
	priv := ed25519.NewKeyFromSeed(seed)
	s.key.Store(&signingKey{priv: priv, kid: crypto.KeyID(priv.Public().(ed25519.PublicKey))})
}

// PublicKey returns the verification half, or nil when no key is installed.
func (s *TokenSigner) PublicKey() ed25519.PublicKey {
	k := s.key.Load()
	if k == nil {
		return nil
	}
	return k.priv.Public().(ed25519.PublicKey)
}

// SetPreviousPublicKey records the key a rotation retired. Call it after
// SetSeed, which clears it.
func (s *TokenSigner) SetPreviousPublicKey(pub ed25519.PublicKey) {
	if len(pub) == 0 {
		s.prev.Store(nil)
		return
	}
	s.prev.Store(&pub)
}

// verificationKeys are the keys a live token may have been signed with.
func (s *TokenSigner) verificationKeys() []ed25519.PublicKey {
	var out []ed25519.PublicKey
	if pub := s.PublicKey(); pub != nil {
		out = append(out, pub)
	}
	if prev := s.prev.Load(); prev != nil {
		out = append(out, *prev)
	}
	return out
}

// ErrNoSigningKey means the deployment has no token signing key — tokens
// cannot be minted until one is configured.
var ErrNoSigningKey = errors.New("control: no token signing key")

func (s *TokenSigner) sign(claims crypto.TokenClaims) (string, error) {
	k := s.key.Load()
	if k == nil {
		return "", ErrNoSigningKey
	}
	return crypto.SignToken(k.priv, k.kid, claims)
}

// rotateTokenKey re-keys the signer. Deployment-wide and unscoped, so it is
// gated on settings.update rather than a tenant verb.
func rotateTokenKey(ctx context.Context, d Deps) (*emptyOutput, error) {
	if err := d.Authz.Authorize(ctx, "settings.update", authz.Resource{Kind: "settings"}); err != nil {
		return nil, mapAuthzErr(err)
	}
	if d.RotateTokenKey == nil {
		return nil, huma.Error503ServiceUnavailable("tokens_disabled: no signing-key store is configured")
	}
	if err := d.RotateTokenKey(ctx); err != nil {
		return nil, huma.Error500InternalServerError("signing-key rotation failed: " + err.Error())
	}
	audit.Record(ctx, "tokens.rotate", audit.Resource{Kind: "token", Name: "signing-key"}, audit.StatusAllowed)
	return &emptyOutput{}, nil
}
