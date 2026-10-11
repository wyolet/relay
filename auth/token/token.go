// Package token signs and verifies Ed25519 compact JWTs whose header may name
// the verification key by `kid`. The claim set is the caller's: any value that
// encodes to a JSON object.
//
// Claim policy (issuer, audience, expiry, versioning) and verification
// caching are the caller's. Other JOSE algorithms are out of scope.
package token

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

// ErrMalformed is returned for anything that isn't a well-formed EdDSA JWT;
// ErrSignature for a well-formed one that doesn't verify.
var (
	ErrMalformed = errors.New("token: malformed token")
	ErrSignature = errors.New("token: signature does not verify")
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

// Sign returns the compact JWT for claims, signed with priv. kid names the
// verification key so a rotation can keep the previous one live; an empty
// kid writes the bare header.
func Sign(priv ed25519.PrivateKey, kid string, claims any) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("token: signing key is %d bytes, want %d", len(priv), ed25519.PrivateKeySize)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("token: encode claims: %w", err)
	}
	header := jwtHeader
	if kid != "" {
		if strings.ContainsAny(kid, `"\`) {
			return "", fmt.Errorf("token: kid must not contain quotes or backslashes")
		}
		header = base64url([]byte(`{"alg":"EdDSA","typ":"JWT","kid":"` + kid + `"}`))
	}
	signing := header + "." + base64url(payload)
	return signing + "." + base64url(ed25519.Sign(priv, []byte(signing))), nil
}

// header is the JOSE header this package writes and reads. Only these three
// members are ever present.
type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid,omitempty"`
}

// HeaderKeyID reports the `kid` a token names, or "" when it carries none.
// Reads the header only — nothing here is a trust decision.
func HeaderKeyID(token string) string {
	seg, _, ok := strings.Cut(token, ".")
	if !ok || seg == jwtHeader {
		return ""
	}
	raw, err := rawURL.DecodeString(seg)
	if err != nil {
		return ""
	}
	var h header
	if json.Unmarshal(raw, &h) != nil {
		return ""
	}
	return h.Kid
}

// Parse verifies the signature with pub and decodes the payload into C. It
// checks nothing else — expiry, issuer and version are the caller's policy,
// not the format's.
func Parse[C any](pub ed25519.PublicKey, token string) (C, error) {
	var claims C
	if len(pub) != ed25519.PublicKeySize {
		return claims, fmt.Errorf("token: verification key is %d bytes, want %d", len(pub), ed25519.PublicKeySize)
	}
	head, rest, ok := strings.Cut(token, ".")
	if !ok {
		return claims, ErrMalformed
	}
	payload, sig, ok := strings.Cut(rest, ".")
	if !ok || !validHeader(head) {
		return claims, ErrMalformed
	}
	rawSig, err := rawURL.DecodeString(sig)
	if err != nil {
		return claims, ErrMalformed
	}
	if !ed25519.Verify(pub, []byte(head+"."+payload), rawSig) {
		return claims, ErrSignature
	}
	rawPayload, err := rawURL.DecodeString(payload)
	if err != nil {
		return claims, ErrMalformed
	}
	if err := json.Unmarshal(rawPayload, &claims); err != nil {
		var zero C
		return zero, ErrMalformed
	}
	return claims, nil
}

// validHeader accepts the bare header verbatim and otherwise insists on
// EdDSA/JWT: `alg` is a trust decision, so it is never taken on faith.
func validHeader(seg string) bool {
	if seg == jwtHeader {
		return true
	}
	// A byte scan rather than a JSON decode: this runs per verification and
	// the header is a fixed shape this package itself writes.
	raw, err := rawURL.DecodeString(seg)
	if err != nil || len(raw) > maxHeaderBytes {
		return false
	}
	s := string(raw)
	return strings.Contains(s, `"alg":"EdDSA"`) && strings.Contains(s, `"typ":"JWT"`)
}

// maxHeaderBytes bounds the JOSE header a token may carry. The one Sign
// writes is well under 100 bytes; anything larger is not one of ours.
const maxHeaderBytes = 256

func base64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// rawURL decodes strictly: a segment whose length leaves unused trailing bits
// (an Ed25519 signature leaves four) has many encodings of the same bytes, so
// a non-strict decode would verify one token under several distinct strings.
var rawURL = base64.RawURLEncoding.Strict()
