// token.go is the data plane's half of inference tokens: hold the public
// key, verify a bearer against it, and turn the claims into a Principal.
// Minting lives on the control plane; nothing here reads Postgres.
package inference

import (
	"crypto/ed25519"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/pkg/crypto"
)

// TokenVerifier holds the public half of the token signing key. The
// composition root swaps the key in when the auth:tokens section changes; a
// nil verifier (or one with no key) rejects every token, which is what a
// deployment with tokens disabled wants.
type TokenVerifier struct {
	mu  sync.RWMutex
	pub ed25519.PublicKey
}

// SetKey installs the verification key. A nil key disables tokens.
func (v *TokenVerifier) SetKey(pub ed25519.PublicKey) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.pub = pub
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
	v.mu.RLock()
	pub := v.pub
	v.mu.RUnlock()
	if len(pub) == 0 {
		return crypto.TokenClaims{}, ErrTokensDisabled
	}
	return crypto.ParseToken(pub, token)
}

// looksLikeToken classifies a bearer by shape: a compact JWT is three
// base64url segments whose header starts "ey". No key format can collide —
// keys carry no dots.
func looksLikeToken(bearer string) bool {
	return strings.HasPrefix(bearer, "ey") && strings.Count(bearer, ".") == 2
}

// tokenPrincipal verifies a token bearer and resolves it against the
// snapshot, writing the response and reporting false on any rejection.
func tokenPrincipal(w http.ResponseWriter, snap *appcatalog.Snapshot, tokens *TokenVerifier, bearer string) (*Principal, bool) {
	claims, err := tokens.Verify(bearer)
	if err != nil {
		writeAuthErr(w, "invalid token")
		return nil, false
	}
	userID := claims.UserID()
	now := time.Now().Unix()
	switch {
	case claims.Iss != crypto.TokenIssuer, userID == "":
		writeAuthErr(w, "invalid token")
		return nil, false
	case claims.Exp <= now:
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
		Subjects:       appcatalog.UserSubjects(userID, snap.GroupsForUser(userID), claims.Grp),
		UserID:         userID,
		ProjectID:      proj.Meta.ID,
		TeamID:         proj.Spec.TeamID,
		CredentialKind: CredentialToken,
		CredentialID:   claims.Jti,
	}, true
}
