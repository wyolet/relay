package crypto

import (
	"context"
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

// The key set must mint and verify exactly what the bare functions do, so a
// token in circulation is unaffected by which path minted or checks it.
func TestTokenClaims_KeySetMatchesTheFixture(t *testing.T) {
	fx := readTokenFixture(t)
	ctx := context.Background()
	priv := ed25519.NewKeyFromSeed([]byte(compatSeed))
	other, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}

	// As the current key, and as the previous one after a rotation.
	current := authtoken.NewKeySet(authtoken.NewStaticStore(authtoken.Keys{Signing: priv}), authtoken.KeySetOptions{})
	rotated := authtoken.NewKeySet(authtoken.NewStaticStore(authtoken.Keys{
		Current: other, Previous: []ed25519.PublicKey{priv.Public().(ed25519.PublicKey)},
	}), authtoken.KeySetOptions{MinReloadInterval: -1})
	for _, ks := range []*authtoken.KeySet{current, rotated} {
		if err := ks.Reload(ctx); err != nil {
			t.Fatalf("Reload: %v", err)
		}
	}

	minted, err := current.Sign(compatClaims())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if minted != fx["with-kid"] {
		t.Errorf("key set minted a token that differs from the fixture:\n got %s\nwant %s", minted, fx["with-kid"])
	}
	for _, tc := range []struct {
		name string
		ks   *authtoken.KeySet
		ok   bool
	}{
		{"with-kid", current, true},
		{"bare", current, true},
		{"with-kid", rotated, true},
		// A token with no kid has only ever been checked against the current key.
		{"bare", rotated, false},
	} {
		claims, err := authtoken.Verify[TokenClaims](ctx, tc.ks, fx[tc.name])
		if !tc.ok {
			if err == nil {
				t.Errorf("%s verified against a key set it was not minted under as current", tc.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: Verify: %v", tc.name, err)
		} else if !reflect.DeepEqual(claims, compatClaims()) {
			t.Errorf("%s: claims = %+v, want %+v", tc.name, claims, compatClaims())
		}
	}
	set, err := current.JWKSet()
	if err != nil || len(set.Keys) != 1 || set.Keys[0].Kid != fx["kid"] {
		t.Errorf("JWKSet = %+v, %v; want one key whose kid is the fixture's %q", set, err, fx["kid"])
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
