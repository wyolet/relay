package crypto

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

const base64urlAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// nonCanonicalTwin rewrites seg's final character into a different one that
// decodes to the same bytes, by incrementing the trailing bits the segment's
// length leaves unused.
func nonCanonicalTwin(t *testing.T, seg string) string {
	t.Helper()
	unused := map[int]int{2: 4, 3: 2}[len(seg)%4]
	if unused == 0 {
		t.Fatalf("segment of %d chars leaves no unused bits", len(seg))
	}
	last := strings.IndexByte(base64urlAlphabet, seg[len(seg)-1])
	if last < 0 {
		t.Fatalf("segment ends in %q, not a base64url character", seg[len(seg)-1])
	}
	mask := 1<<unused - 1
	twin := seg[:len(seg)-1] + string(base64urlAlphabet[last&^mask|(last&mask+1)&mask])

	want, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatalf("decode segment: %v", err)
	}
	got, err := base64.RawURLEncoding.DecodeString(twin)
	if err != nil {
		t.Fatalf("decode twin: %v", err)
	}
	if string(got) != string(want) {
		t.Fatal("twin decodes to different bytes than the segment")
	}
	return twin
}

func canonicalTestClaims() TokenClaims {
	return TokenClaims{
		Iss: TokenIssuer,
		Sub: "user:019200aa",
		Prj: "019200bb",
		Ver: 1,
		Jti: "019200cc",
		Iat: time.Now().Unix(),
		Exp: time.Now().Add(time.Hour).Unix(),
	}
}

func TestParseToken_AcceptsCanonicalEncoding(t *testing.T) {
	pub, priv := testKey(t)
	token, err := SignToken(priv, "", canonicalTestClaims())
	if err != nil {
		t.Fatalf("SignToken: %v", err)
	}
	if _, err := ParseToken(pub, token); err != nil {
		t.Fatalf("ParseToken: %v", err)
	}
}

func TestParseToken_RejectsNonCanonicalSignature(t *testing.T) {
	pub, priv := testKey(t)
	token, err := SignToken(priv, "", canonicalTestClaims())
	if err != nil {
		t.Fatalf("SignToken: %v", err)
	}
	header, rest, _ := strings.Cut(token, ".")
	payload, sig, _ := strings.Cut(rest, ".")

	twin := header + "." + payload + "." + nonCanonicalTwin(t, sig)
	if _, err := ParseToken(pub, twin); !errors.Is(err, ErrTokenMalformed) {
		t.Fatalf("ParseToken on a non-canonically encoded signature: %v, want ErrTokenMalformed", err)
	}
}

func TestParseToken_RejectsNonCanonicalPayload(t *testing.T) {
	pub, priv := testKey(t)
	claims := canonicalTestClaims()
	// Grow the subject until the encoded payload's length leaves unused bits
	// to flip; the claim's content is irrelevant to the property.
	var payload string
	for range 4 {
		token, err := SignToken(priv, "", claims)
		if err != nil {
			t.Fatalf("SignToken: %v", err)
		}
		_, rest, _ := strings.Cut(token, ".")
		payload, _, _ = strings.Cut(rest, ".")
		if len(payload)%4 != 0 {
			break
		}
		claims.Sub += "a"
	}
	if len(payload)%4 == 0 {
		t.Fatal("no payload length leaving unused bits")
	}

	// Signed over the mutated segment: without a strict decode the token both
	// verifies and yields the same claims as the canonical one.
	signing := jwtHeader + "." + nonCanonicalTwin(t, payload)
	token := signing + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, []byte(signing)))
	if _, err := ParseToken(pub, token); !errors.Is(err, ErrTokenMalformed) {
		t.Fatalf("ParseToken on a non-canonically encoded payload: %v, want ErrTokenMalformed", err)
	}
}
