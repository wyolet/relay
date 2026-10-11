package token

import (
	"crypto/ed25519"
	"fmt"
)

// PublicJWK is an Ed25519 public key as an OKP JSON Web Key (RFC 8037). Its
// field order is fixed, so the encoding is deterministic.
type PublicJWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	// Kid equals KeyID, the value a token's header names.
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
}

// PublicJWKSet is a JWK Set (RFC 7517 §5), the document a `jwks_uri` serves.
type PublicJWKSet struct {
	Keys []PublicJWK `json:"keys"`
}

// JWK returns pub as a signature-verification JWK.
func JWK(pub ed25519.PublicKey) (PublicJWK, error) {
	if len(pub) != ed25519.PublicKeySize {
		return PublicJWK{}, fmt.Errorf("token: public key is %d bytes, want %d", len(pub), ed25519.PublicKeySize)
	}
	return PublicJWK{Kty: "OKP", Crv: "Ed25519", X: base64url(pub), Kid: KeyID(pub), Use: "sig", Alg: "EdDSA"}, nil
}

// JWKSet returns the keys in the order given, each kid once. With no keys it
// is an empty set, never null.
func JWKSet(pubs ...ed25519.PublicKey) (PublicJWKSet, error) {
	set := PublicJWKSet{Keys: make([]PublicJWK, 0, len(pubs))}
	for _, pub := range pubs {
		k, err := JWK(pub)
		if err != nil {
			return PublicJWKSet{}, err
		}
		if !set.has(k.Kid) {
			set.Keys = append(set.Keys, k)
		}
	}
	return set, nil
}

func (s PublicJWKSet) has(kid string) bool {
	for _, k := range s.Keys {
		if k.Kid == kid {
			return true
		}
	}
	return false
}
