// Token verification runs on the request path, so it reads only the held
// public key and the snapshot, never Postgres. Minting is control-plane only.

package inference

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wyolet/relay/app/plan"
	authtoken "github.com/wyolet/relay/auth/token"
	"github.com/wyolet/relay/pkg/crypto"
)

// TokenVerifier holds the public half of the token signing key, plus the one
// rotated out, so a rotation is not a global logout. The composition root
// swaps the keys in when the auth:tokens section changes; a nil verifier (or
// one with no key) rejects every token, which is what a deployment with
// tokens disabled wants.
type TokenVerifier struct {
	init  sync.Once
	store *authtoken.StaticStore
	keys  *authtoken.KeySet
	// mu serializes key changes: SetKey reads the set it replaces.
	mu    sync.Mutex
	cache claimsCache
}

func (v *TokenVerifier) keySet() *authtoken.KeySet {
	v.init.Do(func() {
		v.store = &authtoken.StaticStore{}
		// The settings section pushes every key change here, so an unknown
		// kid has nothing new to find in the store.
		v.keys = authtoken.NewKeySet(v.store, authtoken.KeySetOptions{MinReloadInterval: -1})
	})
	return v.keys
}

// install replaces the key set. A key of the wrong size verifies nothing, so
// it is left out rather than installed.
func (v *TokenVerifier) install(current, previous ed25519.PublicKey) {
	ks := v.keySet()
	var next authtoken.Keys
	if len(current) == ed25519.PublicKeySize {
		next.Current = current
		if len(previous) == ed25519.PublicKeySize {
			next.Previous = []ed25519.PublicKey{previous}
		}
	}
	v.store.Set(next)
	if err := ks.Reload(context.Background()); err != nil {
		// Unreachable with the sizes checked above; fail closed regardless.
		v.store.Set(authtoken.Keys{})
		_ = ks.Reload(context.Background())
	}
}

// SetKey installs the verification key, demoting the one it replaces so
// tokens already minted under it keep verifying. A nil key disables tokens
// and drops both.
func (v *TokenVerifier) SetKey(pub ed25519.PublicKey) {
	defer v.cache.clear()
	v.mu.Lock()
	defer v.mu.Unlock()
	var previous ed25519.PublicKey
	if old := v.keySet().Current(); old != nil && !old.Equal(pub) {
		previous = old
	}
	v.install(pub, previous)
}

// SetKeys installs the current key alongside the one rotation replaced, so
// a pod that did not perform the rotation still verifies tokens minted
// before it. A nil current key disables tokens and drops both.
func (v *TokenVerifier) SetKeys(current, previous ed25519.PublicKey) {
	if len(previous) == 0 {
		v.SetKey(current)
		return
	}
	defer v.cache.clear()
	v.mu.Lock()
	defer v.mu.Unlock()
	v.install(current, previous)
}

// SetCacheSize bounds the verified-claims cache. 0 turns it off.
func (v *TokenVerifier) SetCacheSize(n int) {
	if v == nil {
		return
	}
	v.cache.resize(n)
}

// PublicKey returns the key tokens are currently minted under, or nil.
func (v *TokenVerifier) PublicKey() ed25519.PublicKey {
	if v == nil {
		return nil
	}
	return v.keySet().Current()
}

// PreviousPublicKey returns the key a rotation retired, or nil.
func (v *TokenVerifier) PreviousPublicKey() ed25519.PublicKey {
	if v == nil {
		return nil
	}
	if prev := v.keySet().Keys().Previous; len(prev) > 0 {
		return prev[0]
	}
	return nil
}

// ErrTokensDisabled means no signing key is configured, so no token can be
// trusted.
var ErrTokensDisabled = errors.New("inference: tokens are not enabled")

// Verify checks the signature and returns the claims. Claim policy (expiry,
// issuer, version) is the caller's.
func (v *TokenVerifier) Verify(token string) (crypto.TokenClaims, error) {
	claims, _, err := v.verify(token)
	return claims, err
}

// verify also returns the header's typ, read in the same decode as the kid
// so checking it costs nothing extra.
func (v *TokenVerifier) verify(token string) (crypto.TokenClaims, string, error) {
	if v == nil {
		return crypto.TokenClaims{}, "", ErrTokensDisabled
	}
	ks := v.keySet()
	if ks.Current() == nil {
		return crypto.TokenClaims{}, "", ErrTokensDisabled
	}
	kid, typ := authtoken.ReadHeader(token)
	// Reloads are off on this key set, so the context is never waited on.
	pub, err := ks.PublicKey(context.Background(), kid)
	if err != nil {
		return crypto.TokenClaims{}, "", err
	}
	claims, err := authtoken.Parse[crypto.TokenClaims](pub, token)
	return claims, typ, err
}

// looksLikeToken classifies a bearer by shape: a compact JWT is three
// base64url segments whose header starts "ey". No key format can collide —
// keys carry no dots.
func looksLikeToken(bearer string) bool {
	return strings.HasPrefix(bearer, "ey") && strings.Count(bearer, ".") == 2
}

// tokenPrincipal verifies a token bearer and resolves it against the
// published plans, writing the response and reporting false on any rejection.
//
// The signature check is the expensive half and its result is cached until
// the token expires; the version check and the revoked-token set are not —
// those are the revocation paths and run on every request, in memory.
func tokenPrincipal(w http.ResponseWriter, a *Auth, root *plan.Root, bearer string) (*Principal, bool) {
	now := time.Now()
	ent, ok := a.Tokens.verified(bearer, now)
	if !ok {
		writeAuthErr(w, "invalid token")
		return nil, false
	}
	return tokenClaimsPrincipal(w, a, root, ent.claims, ent.typ, now)
}

// tokenClaimsPrincipal checks verified claims against the plans and the revoked set, the part of token authentication a long-lived connection repeats per frame.
func tokenClaimsPrincipal(w http.ResponseWriter, a *Auth, root *plan.Root, claims crypto.TokenClaims, typ string, now time.Time) (*Principal, bool) {
	userID := claims.UserID()
	// iat stays out: tokens minted on another pod would fail on clock skew,
	// which relay has never checked. Validate checks typ and iss before exp,
	// so a foreign token reads as invalid, not expired. Relay mints only JWT;
	// an at+jwt is an access token issued for some other resource.
	err := authtoken.Validate(authtoken.Claims{Issuer: claims.Iss, Expiry: claims.Exp, Type: typ}, now,
		authtoken.Want{Issuer: crypto.TokenIssuer, Type: authtoken.TypeJWT})
	switch {
	case userID == "", err != nil && !errors.Is(err, authtoken.ErrExpired):
		writeAuthErr(w, "invalid token")
		return nil, false
	case err != nil:
		writeAuthErr(w, "token expired")
		return nil, false
	}
	// A version mismatch (or an unknown user) is the bulk-revocation path:
	// the user signed out everywhere after this token was minted.
	up := root.Users.Lookup(userID)
	if up == nil || up.TokenVersion != claims.Ver {
		writeAuthErr(w, msgTokenRevoked)
		return nil, false
	}
	if a.Revoked.Revoked(claims.Jti, now) {
		writeAPIError(w, http.StatusUnauthorized, "invalid_request_error", "token_revoked", "this token has been revoked")
		return nil, false
	}
	proj := root.Projects.Lookup(claims.Prj)
	if proj == nil {
		writeForbidden(w, "project_unavailable", "the token's project is not available")
		return nil, false
	}
	return &Principal{
		UserID:         userID,
		ProjectID:      proj.Meta.ID,
		TeamID:         proj.Spec.TeamID,
		CredentialKind: CredentialToken,
		CredentialID:   claims.Jti,
		TokenExp:       claims.Exp,
		TokenVer:       claims.Ver,
		Caller:         up.Caller,
		claims:         claims,
		claimsTyp:      typ,
	}, true
}

// verified returns the token's claims, from the cache when the same bearer
// verified before and has not expired.
func (v *TokenVerifier) verified(bearer string, now time.Time) (*cacheEntry, bool) {
	if v == nil {
		return nil, false
	}
	sum := sha256.Sum256([]byte(bearer))
	digest := hex.EncodeToString(sum[:])
	if ent := v.cache.get(digest, now); ent != nil {
		return ent, true
	}
	claims, typ, err := v.verify(bearer)
	if err != nil {
		return nil, false
	}
	ent := &cacheEntry{claims: claims, typ: typ, exp: time.Unix(claims.Exp, 0)}
	v.cache.put(digest, ent)
	return ent, true
}
