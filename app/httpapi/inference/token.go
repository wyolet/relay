// token.go is the data plane's half of inference tokens: hold the public
// key, verify a bearer against it, and turn the claims into a Principal.
// Minting lives on the control plane; nothing here reads Postgres.
package inference

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/pkg/crypto"
)

// verifyKeys is the key set one verification reads: the key tokens are
// minted under now, plus the one rotated out, so a rotation is not a global
// logout. Replaced wholesale, never mutated.
type verifyKeys struct {
	current, previous       ed25519.PublicKey
	currentKID, previousKID string
}

// TokenVerifier holds the public half of the token signing key. The
// composition root swaps the key in when the auth:tokens section changes; a
// nil verifier (or one with no key) rejects every token, which is what a
// deployment with tokens disabled wants.
type TokenVerifier struct {
	keys  atomic.Pointer[verifyKeys]
	cache claimsCache
}

// SetKey installs the verification key, demoting the one it replaces so
// tokens already minted under it keep verifying. A nil key disables tokens
// and drops both.
func (v *TokenVerifier) SetKey(pub ed25519.PublicKey) {
	defer v.cache.clear()
	if len(pub) == 0 {
		v.keys.Store(nil)
		return
	}
	next := &verifyKeys{current: pub, currentKID: crypto.KeyID(pub)}
	if old := v.keys.Load(); old != nil && !old.current.Equal(pub) {
		next.previous, next.previousKID = old.current, old.currentKID
	}
	v.keys.Store(next)
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
	k := v.keys.Load()
	if k == nil {
		return nil
	}
	return k.current
}

// ErrTokensDisabled means no signing key is configured, so no token can be
// trusted.
var ErrTokensDisabled = errors.New("inference: tokens are not enabled")

// Verify checks the signature and returns the claims. Claim policy (expiry,
// issuer, version) is the caller's.
func (v *TokenVerifier) Verify(token string) (crypto.TokenClaims, error) {
	if v == nil {
		return crypto.TokenClaims{}, ErrTokensDisabled
	}
	k := v.keys.Load()
	if k == nil || len(k.current) == 0 {
		return crypto.TokenClaims{}, ErrTokensDisabled
	}
	pub, ok := k.pick(crypto.TokenKeyID(token))
	if !ok {
		return crypto.TokenClaims{}, crypto.ErrTokenSignature
	}
	return crypto.ParseToken(pub, token)
}

// pick resolves the key a token's kid names. A token with no kid predates
// the rotation support and is treated as minted under the current key.
func (k *verifyKeys) pick(kid string) (ed25519.PublicKey, bool) {
	switch {
	case kid == "", kid == k.currentKID:
		return k.current, true
	case kid == k.previousKID && len(k.previous) > 0:
		return k.previous, true
	}
	return nil, false
}

// looksLikeToken classifies a bearer by shape: a compact JWT is three
// base64url segments whose header starts "ey". No key format can collide —
// keys carry no dots.
func looksLikeToken(bearer string) bool {
	return strings.HasPrefix(bearer, "ey") && strings.Count(bearer, ".") == 2
}

// tokenPrincipal verifies a token bearer and resolves it against the
// snapshot, writing the response and reporting false on any rejection.
//
// The signature check is the expensive half and its result is cached until
// the token expires; the version check and (downstream, in the reserve
// script) the jti denylist are not — those are the revocation paths and run
// on every request.
func tokenPrincipal(w http.ResponseWriter, snap *appcatalog.Snapshot, tokens *TokenVerifier, bearer string) (*Principal, bool) {
	now := time.Now()
	ent, ok := tokens.verified(bearer, now)
	if !ok {
		writeAuthErr(w, "invalid token")
		return nil, false
	}
	claims := ent.claims
	userID := claims.UserID()
	switch {
	case claims.Iss != crypto.TokenIssuer, userID == "":
		writeAuthErr(w, "invalid token")
		return nil, false
	case claims.Exp <= now.Unix():
		writeAuthErr(w, "token expired")
		return nil, false
	}
	// A version mismatch (or an unknown user) is the bulk-revocation path:
	// the user signed out everywhere after this token was minted.
	if ver, ok := snap.TokenVersion(userID); !ok || ver != claims.Ver {
		writeAuthErr(w, "token revoked")
		return nil, false
	}
	proj, ok := snap.Project(claims.Prj)
	if !ok {
		writeForbidden(w, "project_unavailable", "the token's project is not available")
		return nil, false
	}
	return &Principal{
		// A token is stateless: its groups come from the claims it was
		// minted with, unioned with the membership the snapshot holds now.
		Subjects:       tokens.subjects(ent, snap, userID, claims.Grp),
		UserID:         userID,
		ProjectID:      proj.Meta.ID,
		TeamID:         proj.Spec.TeamID,
		CredentialKind: CredentialToken,
		CredentialID:   claims.Jti,
		TokenExp:       claims.Exp,
		TokenVer:       claims.Ver,
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
	claims, err := v.Verify(bearer)
	if err != nil {
		return nil, false
	}
	ent := &cacheEntry{claims: claims, exp: time.Unix(claims.Exp, 0)}
	v.cache.put(digest, ent)
	return ent, true
}

// subjects returns the subject list the claims resolve to, reusing the
// cached one while the snapshot it was derived from is still live. A
// membership change publishes a new snapshot, so it is picked up at once
// rather than at token expiry.
func (v *TokenVerifier) subjects(ent *cacheEntry, snap *appcatalog.Snapshot, userID string, idpGroups []string) []string {
	if got, ok := ent.subjectsFor(snap); ok {
		return got
	}
	subs := appcatalog.UserSubjects(userID, snap.GroupsForUser(userID), idpGroups)
	ent.setSubjects(snap, subs)
	return subs
}
