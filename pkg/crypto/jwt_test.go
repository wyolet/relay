package crypto

import (
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
	"time"
)

func testKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return pub, priv
}

func TestSignToken_RoundTrip(t *testing.T) {
	pub, priv := testKey(t)
	want := TokenClaims{
		Iss: TokenIssuer,
		Sub: "user:019200aa",
		Prj: "019200bb",
		Grp: []string{"platform-eng", "data-science"},
		Ver: 3,
		Jti: "019200cc",
		Iat: time.Now().Unix(),
		Exp: time.Now().Add(time.Hour).Unix(),
	}

	token, err := SignToken(priv, "", want)
	if err != nil {
		t.Fatalf("SignToken: %v", err)
	}
	if n := strings.Count(token, "."); n != 2 {
		t.Fatalf("token has %d dots, want 2 (compact JWT)", n)
	}
	got, err := ParseToken(pub, token)
	if err != nil {
		t.Fatalf("ParseToken: %v", err)
	}
	if got.Sub != want.Sub || got.Prj != want.Prj || got.Ver != want.Ver || got.Jti != want.Jti ||
		got.Iat != want.Iat || got.Exp != want.Exp || strings.Join(got.Grp, ",") != strings.Join(want.Grp, ",") {
		t.Errorf("claims round-trip = %+v, want %+v", got, want)
	}
	if got.UserID() != "019200aa" {
		t.Errorf("UserID = %q", got.UserID())
	}
}

func TestParseToken_Rejections(t *testing.T) {
	pub, priv := testKey(t)
	otherPub, _ := testKey(t)
	token, err := SignToken(priv, "", TokenClaims{Iss: TokenIssuer, Sub: "user:u1", Jti: "j1"})
	if err != nil {
		t.Fatalf("SignToken: %v", err)
	}

	for _, tc := range []struct {
		name  string
		key   ed25519.PublicKey
		token string
		want  error
	}{
		{name: "another key's signature", key: otherPub, token: token, want: ErrTokenSignature},
		{name: "tampered payload", key: pub, token: tamper(token), want: ErrTokenSignature},
		{name: "not a jwt", key: pub, token: "sk-wr-not-a-token", want: ErrTokenMalformed},
		{name: "two segments", key: pub, token: "a.b", want: ErrTokenMalformed},
		{name: "foreign header", key: pub, token: "eyJhbGciOiJIUzI1NiJ9.e30.sig", want: ErrTokenMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseToken(tc.key, tc.token); !errors.Is(err, tc.want) {
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
