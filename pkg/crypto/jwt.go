// jwt.go carries the inference token's wire format: a compact EdDSA-signed
// JWT. The claim set lives here (not in a caller) so the minting and the
// verifying side can never drift apart. Signing and verification are pure —
// callers hold the key material.
package crypto

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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

// KeyID names a public key in a token's `kid` header. Truncated sha256 of
// the key bytes: stable across processes, and it reveals nothing the token
// signature doesn't already.
func KeyID(pub ed25519.PublicKey) string {
	if len(pub) == 0 {
		return ""
	}
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// SignToken returns the compact JWT for claims, signed with priv. kid names
// the verification key so a rotation can keep the previous one live; an
// empty kid writes the bare header.
func SignToken(priv ed25519.PrivateKey, kid string, claims TokenClaims) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("crypto: signing key is %d bytes, want %d", len(priv), ed25519.PrivateKeySize)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("crypto: encode claims: %w", err)
	}
	header := jwtHeader
	if kid != "" {
		if strings.ContainsAny(kid, `"\`) {
			return "", fmt.Errorf("crypto: kid must not contain quotes or backslashes")
		}
		header = base64url([]byte(`{"alg":"EdDSA","typ":"JWT","kid":"` + kid + `"}`))
	}
	signing := header + "." + base64url(payload)
	return signing + "." + base64url(ed25519.Sign(priv, []byte(signing))), nil
}

// tokenHeader is the JOSE header relay writes and reads. Only these three
// members are ever present.
type tokenHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid,omitempty"`
}

// TokenKeyID reports the `kid` a token names, or "" when it carries none.
// Reads the header only — nothing here is a trust decision.
func TokenKeyID(token string) string {
	header, _, ok := strings.Cut(token, ".")
	if !ok || header == jwtHeader {
		return ""
	}
	raw, err := rawURL.DecodeString(header)
	if err != nil {
		return ""
	}
	var h tokenHeader
	if json.Unmarshal(raw, &h) != nil {
		return ""
	}
	return h.Kid
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
	if !ok || !validHeader(header) {
		return TokenClaims{}, ErrTokenMalformed
	}
	rawSig, err := rawURL.DecodeString(sig)
	if err != nil {
		return TokenClaims{}, ErrTokenMalformed
	}
	if !ed25519.Verify(pub, []byte(header+"."+payload), rawSig) {
		return TokenClaims{}, ErrTokenSignature
	}
	rawPayload, err := rawURL.DecodeString(payload)
	if err != nil {
		return TokenClaims{}, ErrTokenMalformed
	}
	var claims TokenClaims
	if err := json.Unmarshal(rawPayload, &claims); err != nil {
		return TokenClaims{}, ErrTokenMalformed
	}
	return claims, nil
}

// validHeader accepts the bare header verbatim and otherwise insists on
// EdDSA/JWT: `alg` is a trust decision, so it is never taken on faith.
func validHeader(header string) bool {
	if header == jwtHeader {
		return true
	}
	// A byte scan rather than a JSON decode: this runs per verification and
	// the header is a fixed shape relay itself writes.
	raw, err := rawURL.DecodeString(header)
	if err != nil || len(raw) > maxHeaderBytes {
		return false
	}
	s := string(raw)
	return strings.Contains(s, `"alg":"EdDSA"`) && strings.Contains(s, `"typ":"JWT"`)
}

// maxHeaderBytes bounds the JOSE header a token may carry. Relay's own is
// well under 100 bytes; anything larger is not one of ours.
const maxHeaderBytes = 256

func base64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// rawURL decodes strictly: a segment whose length leaves unused trailing bits
// (an Ed25519 signature leaves four) has many encodings of the same bytes, so
// a non-strict decode would verify one token under several distinct strings.
var rawURL = base64.RawURLEncoding.Strict()
