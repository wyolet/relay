package crypto

// jwt_fuzz_test.go fuzzes the token parser. ParseToken is reached by every
// unauthenticated caller of the data plane, so the properties it must hold
// for arbitrary bytes are: never panic, and never return claims for a token
// the verification key did not sign.

import (
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
)

// fuzzKeys returns a deterministic signing pair plus a second public key that
// signed nothing, used to prove the signature is really checked.
func fuzzKeys(t testing.TB) (ed25519.PrivateKey, ed25519.PublicKey, ed25519.PublicKey) {
	t.Helper()
	priv := ed25519.NewKeyFromSeed([]byte("relay-fuzz-seed-0123456789abcdef"))
	other := ed25519.NewKeyFromSeed([]byte("relay-fuzz-seed-fedcba9876543210"))
	return priv, priv.Public().(ed25519.PublicKey), other.Public().(ed25519.PublicKey)
}

func fuzzClaims() TokenClaims {
	return TokenClaims{
		Iss: TokenIssuer,
		Sub: "user:0192aaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Prj: "0192ffff-1111-2222-3333-444444444444",
		Grp: []string{"group:platform"},
		Ver: 3,
		Jti: "0192cccc-dddd-eeee-ffff-000000000000",
		Iat: 1_760_000_000,
		Exp: 1_760_003_600,
	}
}

// FuzzParseToken drives the parser with arbitrary bearers. Any input may be
// rejected; none may panic, and an accepted one must carry a signature this
// key made — checked by re-parsing under a key that signed nothing.
func FuzzParseToken(f *testing.F) {
	priv, pub, other := fuzzKeys(f)

	valid, err := SignToken(priv, KeyID(pub), fuzzClaims())
	if err != nil {
		f.Fatalf("sign seed token: %v", err)
	}
	bare, err := SignToken(priv, "", fuzzClaims())
	if err != nil {
		f.Fatalf("sign bare-header seed token: %v", err)
	}
	header, rest, _ := strings.Cut(valid, ".")
	body, sig, _ := strings.Cut(rest, ".")

	for _, seed := range []string{
		"", ".", "..", "a.b.c",
		valid,
		bare,
		valid + "=",
		valid[:len(valid)-1], // truncated signature
		header + "." + body,  // missing the third segment
		header + "." + body + ".",
		"." + body + "." + sig, // missing header
		base64url([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + body + "." + sig,
		base64url([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + body + "." + sig,
		base64url([]byte(`{"typ":"JWT"}`)) + "." + body + "." + sig,
		base64url([]byte(`{"alg":"EdDSA","typ":"JWT","kid":"deadbeefdeadbeef"}`)) + "." + body + "." + sig,
		base64url([]byte(`{"alg":"EdDSA","typ":"JWT"}`)) + "." + base64url([]byte(`{"iss":"relay"`)) + "." + sig,
		header + "." + base64url([]byte(`{"sub":"user:é☃𝄞","prj":"π"}`)) + "." + sig,
		"ey" + strings.Repeat("A", 300) + "." + body + "." + sig, // header over the byte cap
		header + "." + base64url([]byte(strings.Repeat("x", 64<<10))) + "." + sig,
		"Bearer " + valid,
		"sk-wr-not-a-token-at-all",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, token string) {
		claims, err := ParseToken(pub, token)
		// TokenKeyID reads the same header on every bearer, before any trust
		// decision, so it has to survive the same inputs.
		_ = TokenKeyID(token)
		if err != nil {
			return
		}
		if _, otherErr := ParseToken(other, token); otherErr == nil {
			t.Fatalf("token %q verified under a key that signed nothing", token)
		}
		if !headerDeclaresEdDSA(token) {
			t.Fatalf("accepted token %q whose header does not declare EdDSA", token)
		}
		if id := claims.UserID(); id != "" && !strings.HasPrefix(claims.Sub, "user:") {
			t.Fatalf("UserID %q derived from a non-user subject %q", id, claims.Sub)
		}
	})
}

// headerDeclaresEdDSA re-reads the header independently of validHeader, so an
// accepted token is checked against the intent rather than against the same
// code that admitted it.
func headerDeclaresEdDSA(token string) bool {
	header, _, ok := strings.Cut(token, ".")
	if !ok {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(header)
	if err != nil {
		return false
	}
	return strings.Contains(string(raw), `"alg":"EdDSA"`)
}

// FuzzTokenKeyID pins that reading a kid out of an arbitrary header is a
// total function — it runs on every bearer before the signature is checked.
func FuzzTokenKeyID(f *testing.F) {
	priv, pub, _ := fuzzKeys(f)
	valid, err := SignToken(priv, KeyID(pub), fuzzClaims())
	if err != nil {
		f.Fatalf("sign seed token: %v", err)
	}
	for _, seed := range []string{
		"", ".", valid,
		base64url([]byte(`{"alg":"EdDSA","typ":"JWT","kid":"☃"}`)) + ".x.y",
		base64url([]byte(`{"kid":`)) + ".x.y",
		strings.Repeat("ey", 4096) + ".x.y",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, token string) {
		if kid := TokenKeyID(token); strings.ContainsAny(kid, "\x00") {
			t.Fatalf("kid %q carries a NUL", kid)
		}
	})
}
