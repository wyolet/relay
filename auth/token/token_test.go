package token

import (
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
	"time"
)

// testClaims has the shape of a typical caller's claim set, so the tests and
// benchmarks exercise a realistic payload.
type testClaims struct {
	Iss string   `json:"iss"`
	Sub string   `json:"sub"`
	Prj string   `json:"prj"`
	Grp []string `json:"grp,omitempty"`
	Ver int      `json:"ver"`
	Jti string   `json:"jti"`
	Iat int64    `json:"iat"`
	Exp int64    `json:"exp"`
}

func testKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return pub, priv
}

func TestSign_RoundTrip(t *testing.T) {
	pub, priv := testKey(t)
	want := testClaims{
		Iss: "issuer",
		Sub: "user:019200aa",
		Prj: "019200bb",
		Grp: []string{"platform-eng", "data-science"},
		Ver: 3,
		Jti: "019200cc",
		Iat: time.Now().Unix(),
		Exp: time.Now().Add(time.Hour).Unix(),
	}

	token, err := Sign(priv, "", want)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if n := strings.Count(token, "."); n != 2 {
		t.Fatalf("token has %d dots, want 2 (compact JWT)", n)
	}
	got, err := Parse[testClaims](pub, token)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Sub != want.Sub || got.Prj != want.Prj || got.Ver != want.Ver || got.Jti != want.Jti ||
		got.Iat != want.Iat || got.Exp != want.Exp || strings.Join(got.Grp, ",") != strings.Join(want.Grp, ",") {
		t.Errorf("claims round-trip = %+v, want %+v", got, want)
	}
}

func TestSign_KeyID(t *testing.T) {
	pub, priv := testKey(t)
	kid := KeyID(pub)
	if len(kid) != 16 {
		t.Fatalf("KeyID = %q, want 16 hex chars", kid)
	}
	token, err := Sign(priv, kid, testClaims{Sub: "user:u1"})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if got := HeaderKeyID(token); got != kid {
		t.Errorf("HeaderKeyID = %q, want %q", got, kid)
	}
	if _, err := Parse[testClaims](pub, token); err != nil {
		t.Errorf("Parse with kid header: %v", err)
	}
	if _, err := Sign(priv, `bad"kid`, testClaims{}); err == nil {
		t.Error("Sign accepted a kid with a quote")
	}
	if KeyID(nil) != "" {
		t.Error("KeyID(nil) should be empty")
	}
}

func TestParse_Rejections(t *testing.T) {
	pub, priv := testKey(t)
	otherPub, _ := testKey(t)
	token, err := Sign(priv, "", testClaims{Iss: "issuer", Sub: "user:u1", Jti: "j1"})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	for _, tc := range []struct {
		name  string
		key   ed25519.PublicKey
		token string
		want  error
	}{
		{name: "another key's signature", key: otherPub, token: token, want: ErrSignature},
		{name: "tampered payload", key: pub, token: tamper(token), want: ErrSignature},
		{name: "not a jwt", key: pub, token: "sk-not-a-token", want: ErrMalformed},
		{name: "two segments", key: pub, token: "a.b", want: ErrMalformed},
		{name: "foreign header", key: pub, token: "eyJhbGciOiJIUzI1NiJ9.e30.sig", want: ErrMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse[testClaims](tc.key, tc.token); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// tamper flips one byte of the payload segment, leaving the shape intact.
func tamper(token string) string {
	parts := strings.SplitN(token, ".", 3)
	payload := []byte(parts[1])
	if payload[0] == 'A' {
		payload[0] = 'B'
	} else {
		payload[0] = 'A'
	}
	return parts[0] + "." + string(payload) + "." + parts[2]
}
