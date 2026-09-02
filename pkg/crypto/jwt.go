// jwt.go carries the inference token's wire format: a compact EdDSA-signed
// JWT. The claim set lives here (not in a caller) so the minting and the
// verifying side can never drift apart. Signing and verification are pure —
// callers hold the key material.
package crypto

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// TokenIssuer is the only `iss` a relay-minted token carries.
const TokenIssuer = "relay"

// TokenClaims is the inference token's payload. Every field is required
// except Grp, which is empty for a user in no groups.
type TokenClaims struct {
	Iss string   `json:"iss"`
	Sub string   `json:"sub"` // "user:<id>"
	Prj string   `json:"prj"` // project id
	Grp []string `json:"grp,omitempty"`
	Ver int      `json:"ver"` // the user's token version at mint time
	Jti string   `json:"jti"`
	Iat int64    `json:"iat"`
	Exp int64    `json:"exp"`
}

// UserID returns the user id the "user:<id>" subject names, or "" when the
// subject is malformed or names another principal kind.
func (c TokenClaims) UserID() string {
	id, ok := strings.CutPrefix(c.Sub, "user:")
	if !ok {
		return ""
	}
	return id
}

// ErrTokenMalformed is returned for anything that isn't a well-formed EdDSA
// JWT; ErrTokenSignature for a well-formed one that doesn't verify.
var (
	ErrTokenMalformed = errors.New("crypto: malformed token")
	ErrTokenSignature = errors.New("crypto: token signature does not verify")
)

var jwtHeader = base64url([]byte(`{"alg":"EdDSA","typ":"JWT"}`))

// SignToken returns the compact JWT for claims, signed with priv.
func SignToken(priv ed25519.PrivateKey, claims TokenClaims) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("crypto: signing key is %d bytes, want %d", len(priv), ed25519.PrivateKeySize)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("crypto: encode claims: %w", err)
	}
	signing := jwtHeader + "." + base64url(payload)
	return signing + "." + base64url(ed25519.Sign(priv, []byte(signing))), nil
}

// ParseToken verifies the signature with pub and returns the claims. It
// checks nothing else — expiry, issuer and version are the caller's policy,
// not the format's.
func ParseToken(pub ed25519.PublicKey, token string) (TokenClaims, error) {
	if len(pub) != ed25519.PublicKeySize {
		return TokenClaims{}, fmt.Errorf("crypto: verification key is %d bytes, want %d", len(pub), ed25519.PublicKeySize)
	}
	header, rest, ok := strings.Cut(token, ".")
	if !ok {
		return TokenClaims{}, ErrTokenMalformed
	}
	payload, sig, ok := strings.Cut(rest, ".")
	if !ok || header != jwtHeader {
		return TokenClaims{}, ErrTokenMalformed
	}
	rawSig, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return TokenClaims{}, ErrTokenMalformed
	}
	if !ed25519.Verify(pub, []byte(header+"."+payload), rawSig) {
		return TokenClaims{}, ErrTokenSignature
	}
	rawPayload, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return TokenClaims{}, ErrTokenMalformed
	}
	var claims TokenClaims
	if err := json.Unmarshal(rawPayload, &claims); err != nil {
		return TokenClaims{}, ErrTokenMalformed
	}
	return claims, nil
}

func base64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
