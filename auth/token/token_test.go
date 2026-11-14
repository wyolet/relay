package token

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
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

func mustSign(t *testing.T, priv ed25519.PrivateKey, kid string, claims any) string {
	t.Helper()
	tok, err := Sign(priv, kid, claims)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return tok
}

// signRaw signs a header and payload Sign never writes.
func signRaw(priv ed25519.PrivateKey, header, payload string) string {
	signing := base64url([]byte(header)) + "." + base64url([]byte(payload))
	return signing + "." + base64url(ed25519.Sign(priv, []byte(signing)))
}

func segments(t *testing.T, token string) (header, payload, sig string) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3", len(parts))
	}
	return parts[0], parts[1], parts[2]
}

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
	mask := 1<<unused - 1
	twin := seg[:len(seg)-1] + string(base64urlAlphabet[last&^mask|(last&mask+1)&mask])

	want, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatalf("decode segment: %v", err)
	}
	got, err := base64.RawURLEncoding.DecodeString(twin)
	if err != nil || string(got) != string(want) || twin == seg {
		t.Fatalf("twin %q of %q is not a second encoding of the same bytes (%v)", twin, seg, err)
	}
	return twin
}

func TestKeyID(t *testing.T) {
	pub, _ := testKey(t)
	sum := sha256.Sum256(pub)
	if got, want := KeyID(pub), hex.EncodeToString(sum[:8]); got != want {
		t.Errorf("KeyID = %q, want %q", got, want)
	}
	if got := KeyID(nil); got != "" {
		t.Errorf("KeyID(nil) = %q, want empty", got)
	}
}

func TestSign(t *testing.T) {
	pub, priv := testKey(t)
	claims := testClaims{
		Iss: "issuer", Sub: "user:019200aa", Prj: "019200bb", Grp: []string{"platform-eng", "data-science"},
		Ver: 3, Jti: "019200cc", Iat: 1_760_000_000, Exp: 1_760_003_600,
	}
	const payload = `{"iss":"issuer","sub":"user:019200aa","prj":"019200bb","grp":["platform-eng","data-science"],"ver":3,"jti":"019200cc","iat":1760000000,"exp":1760003600}`

	for _, tc := range []struct {
		name, kid, wantHeader string
	}{
		{"bare header", "", `{"alg":"EdDSA","typ":"JWT"}`},
		{"kid header", KeyID(pub), `{"alg":"EdDSA","typ":"JWT","kid":"` + KeyID(pub) + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok := mustSign(t, priv, tc.kid, claims)
			header, body, sig := segments(t, tok)
			if want := base64url([]byte(tc.wantHeader)); header != want {
				t.Errorf("header = %q, want %q", header, want)
			}
			if want := base64url([]byte(payload)); body != want {
				t.Errorf("payload = %q, want %q", body, want)
			}
			if want := base64url(ed25519.Sign(priv, []byte(header+"."+body))); sig != want {
				t.Errorf("signature = %q, want %q", sig, want)
			}
			if kid, typ := ReadHeader(tok); kid != tc.kid || typ != TypeJWT {
				t.Errorf("ReadHeader = (%q, %q), want (%q, JWT)", kid, typ, tc.kid)
			}
			got, err := Parse[testClaims](pub, tok)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if d := cmp.Diff(claims, got); d != "" {
				t.Errorf("claims round-trip (-want +got):\n%s", d)
			}
		})
	}
}

func TestSign_Errors(t *testing.T) {
	_, priv := testKey(t)
	for _, tc := range []struct {
		name   string
		priv   ed25519.PrivateKey
		kid    string
		claims any
		want   string
	}{
		{"short key", priv[:10], "", testClaims{}, "token: signing key is 10 bytes, want 64"},
		{"nil key", nil, "", testClaims{}, "token: signing key is 0 bytes, want 64"},
		{"kid with a quote", priv, `bad"kid`, testClaims{}, "token: kid must not contain quotes or backslashes"},
		{"kid with a backslash", priv, `bad\kid`, testClaims{}, "token: kid must not contain quotes or backslashes"},
		{"claims that do not encode", priv, "", make(chan int), "token: encode claims: json: unsupported type: chan int"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok, err := Sign(tc.priv, tc.kid, tc.claims)
			if err == nil || err.Error() != tc.want || tok != "" {
				t.Errorf("Sign = (%q, %v), want error %q", tok, err, tc.want)
			}
		})
	}
}

func TestReadHeader(t *testing.T) {
	for _, tc := range []struct {
		name, token, kid, typ string
	}{
		{"bare", jwtHeader + ".e30.sig", "", TypeJWT},
		{"kid and access-token typ", base64url([]byte(`{"alg":"EdDSA","typ":"at+jwt","kid":"k1"}`)) + ".e30.sig", "k1", TypeAccessToken},
		{"foreign typ is reported as written", base64url([]byte(`{"alg":"EdDSA","typ":"dpop+jwt"}`)) + ".e30.sig", "", "dpop+jwt"},
		{"no typ", base64url([]byte(`{"alg":"EdDSA","kid":"k1"}`)) + ".e30.sig", "k1", ""},
		{"no dot", "sk-not-a-token", "", ""},
		{"header is not base64url", "!!!.e30.sig", "", ""},
		{"header is not json", base64url([]byte(`{"kid":"k1"`)) + ".e30.sig", "", ""},
		{"empty", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kid, typ := ReadHeader(tc.token)
			if kid != tc.kid || typ != tc.typ {
				t.Errorf("ReadHeader = (%q, %q), want (%q, %q)", kid, typ, tc.kid, tc.typ)
			}
			if got := HeaderKeyID(tc.token); got != tc.kid {
				t.Errorf("HeaderKeyID = %q, want %q", got, tc.kid)
			}
			if got := HeaderType(tc.token); got != tc.typ {
				t.Errorf("HeaderType = %q, want %q", got, tc.typ)
			}
		})
	}
}

func TestParse_Headers(t *testing.T) {
	pub, priv := testKey(t)
	// 256 bytes is the largest header Parse reads.
	pad := func(n int) string {
		const head = `{"alg":"EdDSA","typ":"JWT","kid":"`
		return head + strings.Repeat("k", n-len(head)-2) + `"}`
	}
	for _, tc := range []struct {
		name, header string
		want         error
	}{
		{"jwt with kid", `{"alg":"EdDSA","typ":"JWT","kid":"k1"}`, nil},
		{"access token", `{"alg":"EdDSA","typ":"at+jwt","kid":"k1"}`, nil},
		{"header at the size cap", pad(maxHeaderBytes), nil},
		{"header over the size cap", pad(maxHeaderBytes + 1), ErrMalformed},
		{"foreign typ", `{"alg":"EdDSA","typ":"dpop+jwt"}`, ErrMalformed},
		{"no typ", `{"alg":"EdDSA"}`, ErrMalformed},
		{"alg none", `{"alg":"none","typ":"JWT"}`, ErrMalformed},
		{"alg HS256", `{"alg":"HS256","typ":"JWT"}`, ErrMalformed},
		{"no alg", `{"typ":"JWT"}`, ErrMalformed},
		{"empty header", ``, ErrMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse[testClaims](pub, signRaw(priv, tc.header, `{"sub":"user:u1"}`))
			if err != tc.want {
				t.Fatalf("Parse = %v, want %v", err, tc.want)
			}
			want := testClaims{}
			if tc.want == nil {
				want.Sub = "user:u1"
			}
			if d := cmp.Diff(want, got); d != "" {
				t.Errorf("claims (-want +got):\n%s", d)
			}
		})
	}
}

func TestParse_Rejections(t *testing.T) {
	pub, priv := testKey(t)
	otherPub, otherPriv := testKey(t)
	claims := testClaims{Iss: "issuer", Sub: "user:u1", Jti: "j1"}
	token := mustSign(t, priv, "", claims)
	header, payload, sig := segments(t, token)
	_, _, otherSig := segments(t, mustSign(t, priv, "", testClaims{Sub: "user:u2"}))
	forged := base64url([]byte(`{"iss":"issuer","sub":"user:admin","prj":"","ver":0,"jti":"j1","iat":0,"exp":0}`))
	flipped, _ := base64.RawURLEncoding.DecodeString(sig)
	flipped[10] ^= 1

	for _, tc := range []struct {
		name  string
		key   ed25519.PublicKey
		token string
		want  error
	}{
		{"another key's signature", otherPub, token, ErrSignature},
		{"signed by another key", pub, mustSign(t, otherPriv, "", claims), ErrSignature},
		{"payload replaced", pub, header + "." + forged + "." + sig, ErrSignature},
		{"header gains a kid after signing", pub, base64url([]byte(`{"alg":"EdDSA","typ":"JWT","kid":"k1"}`)) + "." + payload + "." + sig, ErrSignature},
		{"signature of another token", pub, header + "." + payload + "." + otherSig, ErrSignature},
		{"one signature bit flipped", pub, header + "." + payload + "." + base64url(flipped), ErrSignature},
		{"signature truncated to whole bytes", pub, token[:len(token)-6], ErrSignature},
		{"empty signature", pub, header + "." + payload + ".", ErrSignature},
		{"signature truncated mid-byte", pub, token[:len(token)-1], ErrMalformed},
		{"padded signature", pub, token + "=", ErrMalformed},
		{"second encoding of the same signature", pub, header + "." + payload + "." + nonCanonicalTwin(t, sig), ErrMalformed},
		{"fourth segment", pub, token + ".x", ErrMalformed},
		{"no header", pub, "." + payload + "." + sig, ErrMalformed},
		{"two segments", pub, header + "." + payload, ErrMalformed},
		{"not a jwt", pub, "sk-not-a-token", ErrMalformed},
		{"empty", pub, "", ErrMalformed},
		{"signed payload is not json", pub, signRaw(priv, `{"alg":"EdDSA","typ":"JWT"}`, `{"sub":"user:u1"`), ErrMalformed},
		{"signed payload has the wrong field type", pub, signRaw(priv, `{"alg":"EdDSA","typ":"JWT"}`, `{"sub":"user:u1","ver":"three"}`), ErrMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse[testClaims](tc.key, tc.token)
			if err != tc.want {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if d := cmp.Diff(testClaims{}, got); d != "" {
				t.Errorf("a rejected token returned claims (-want +got):\n%s", d)
			}
		})
	}

	_, err := Parse[testClaims](pub[:3], token)
	if err == nil || err.Error() != "token: verification key is 3 bytes, want 32" {
		t.Errorf("Parse with a 3-byte key = %v", err)
	}
}

// Signed over the rewritten segment: without a strict decode the token both
// verifies and yields the same claims as the canonical one.
func TestParse_RejectsSecondEncodingOfPayload(t *testing.T) {
	pub, priv := testKey(t)
	payload := base64url([]byte(`{"sub":"user:u1"}`))
	signing := jwtHeader + "." + nonCanonicalTwin(t, payload)
	token := signing + "." + base64url(ed25519.Sign(priv, []byte(signing)))
	if _, err := Parse[testClaims](pub, token); !errors.Is(err, ErrMalformed) {
		t.Fatalf("Parse = %v, want ErrMalformed", err)
	}
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
