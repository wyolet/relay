package control

import (
	"context"
	"crypto/ed25519"
	"errors"
	"sync"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	authtoken "github.com/wyolet/relay/auth/token"
	"github.com/wyolet/relay/pkg/crypto"
)

// TokenSigner holds the Ed25519 signing key, plus the verification half of
// the key a rotation retired: minting never uses that one, but a token
// presented for revocation may carry it. The composition root swaps the keys
// in when the auth:tokens section changes; an empty signer refuses to mint,
// which is the posture of a deployment with no master key.
type TokenSigner struct {
	init  sync.Once
	store *authtoken.StaticStore
	keys  *authtoken.KeySet
	// mu serializes key changes: each one rewrites the set it reads.
	mu sync.Mutex
}

func (s *TokenSigner) keySet() *authtoken.KeySet {
	s.init.Do(func() {
		s.store = &authtoken.StaticStore{}
		// The settings section pushes every key change here, so an unknown
		// kid has nothing new to find in the store.
		s.keys = authtoken.NewKeySet(s.store, authtoken.KeySetOptions{MinReloadInterval: -1})
	})
	return s.keys
}

// install replaces the key set. The callers size-check every key, so the
// reload cannot fail; if it did, minting would be left off.
func (s *TokenSigner) install(k authtoken.Keys) {
	ks := s.keySet()
	s.store.Set(k)
	if err := ks.Reload(context.Background()); err != nil {
		s.store.Set(authtoken.Keys{})
		_ = ks.Reload(context.Background())
	}
}

// SetSeed installs the signing key from its 32-byte seed. A nil seed
// disables minting.
func (s *TokenSigner) SetSeed(seed []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(seed) != ed25519.SeedSize {
		s.install(authtoken.Keys{})
		return
	}
	s.install(authtoken.Keys{Signing: ed25519.NewKeyFromSeed(seed)})
}

// PublicKey returns the verification half, or nil when no key is installed.
func (s *TokenSigner) PublicKey() ed25519.PublicKey {
	return s.keySet().Keys().Current
}

// SetPreviousPublicKey records the key a rotation retired. Call it after
// SetSeed, which clears it. A key of the wrong size verifies nothing, so it
// is left out.
func (s *TokenSigner) SetPreviousPublicKey(pub ed25519.PublicKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.keySet().Keys()
	k.Previous = nil
	if len(pub) == ed25519.PublicKeySize {
		k.Previous = []ed25519.PublicKey{pub}
	}
	s.install(k)
}

// verificationKeys are the keys a live token may have been signed with.
func (s *TokenSigner) verificationKeys() []ed25519.PublicKey {
	return s.keySet().PublicKeys()
}

// ErrNoSigningKey means the deployment has no token signing key — tokens
// cannot be minted until one is configured.
var ErrNoSigningKey = errors.New("control: no token signing key")

func (s *TokenSigner) sign(claims crypto.TokenClaims) (string, error) {
	tok, err := s.keySet().Sign(claims)
	if errors.Is(err, authtoken.ErrNoSigningKey) {
		return "", ErrNoSigningKey
	}
	return tok, err
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
