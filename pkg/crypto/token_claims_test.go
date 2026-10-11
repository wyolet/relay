package crypto

import (
	"crypto/ed25519"
	"os"
	"reflect"
	"strings"
	"testing"

	authtoken "github.com/wyolet/relay/auth/token"
)

// testdata/tokens.txt holds tokens an earlier release minted from compatSeed
// and compatClaims: tokens already in circulation must keep verifying, and
// minting the same input must stay byte-identical. Never regenerate it.
const compatSeed = "relay-compat-seed-0123456789abcd"

func compatClaims() TokenClaims {
	return TokenClaims{
		Iss: TokenIssuer,
		Sub: "user:0192aaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Prj: "0192ffff-1111-2222-3333-444444444444",
		Grp: []string{"platform-eng", "data-science"},
		Ver: 3,
		Jti: "0192cccc-dddd-eeee-ffff-000000000000",
		Iat: 1_760_000_000,
		Exp: 1_760_003_600,
	}
}

func readTokenFixture(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile("testdata/tokens.txt")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		name, value, ok := strings.Cut(line, " ")
		if !ok {
			t.Fatalf("fixture line %q has no value", line)
		}
		out[name] = value
	}
	return out
}

func TestTokenClaims_PreviouslyMintedTokensVerify(t *testing.T) {
	fx := readTokenFixture(t)
	priv := ed25519.NewKeyFromSeed([]byte(compatSeed))
	pub := priv.Public().(ed25519.PublicKey)

	if got := authtoken.KeyID(pub); got != fx["kid"] {
		t.Fatalf("KeyID = %q, want %q", got, fx["kid"])
	}
	for _, tc := range []struct{ name, kid string }{{"with-kid", fx["kid"]}, {"bare", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			old := fx[tc.name]
			if got := authtoken.HeaderKeyID(old); got != tc.kid {
				t.Errorf("HeaderKeyID = %q, want %q", got, tc.kid)
			}
			claims, err := authtoken.Parse[TokenClaims](pub, old)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if !reflect.DeepEqual(claims, compatClaims()) {
				t.Errorf("claims = %+v, want %+v", claims, compatClaims())
			}
			minted, err := authtoken.Sign(priv, tc.kid, compatClaims())
			if err != nil {
				t.Fatalf("Sign: %v", err)
			}
			if minted != old {
				t.Errorf("minted token differs from the fixture:\n got %s\nwant %s", minted, old)
			}
		})
	}
}

func TestTokenClaims_UserID(t *testing.T) {
	for sub, want := range map[string]string{
		"user:019200aa":           "019200aa",
		"serviceaccount:019200aa": "",
		"019200aa":                "",
		"":                        "",
	} {
		if got := (TokenClaims{Sub: sub}).UserID(); got != want {
			t.Errorf("UserID(%q) = %q, want %q", sub, got, want)
		}
	}
}
