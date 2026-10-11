package token

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestAudience_JSON(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Audience
	}{
		{`"api"`, Audience{"api"}},
		{`["api","web"]`, Audience{"api", "web"}},
		{`[]`, Audience{}},
		{`null`, nil},
	} {
		var c struct {
			Aud Audience `json:"aud"`
		}
		if err := json.Unmarshal([]byte(`{"aud":`+tc.in+`}`), &c); err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if len(c.Aud) != len(tc.want) || (len(c.Aud) > 0 && c.Aud[len(c.Aud)-1] != tc.want[len(tc.want)-1]) {
			t.Errorf("%s decoded to %v, want %v", tc.in, c.Aud, tc.want)
		}
	}
	for _, bad := range []string{`1`, `{"a":1}`, `[1]`} {
		var a Audience
		if err := json.Unmarshal([]byte(bad), &a); err == nil {
			t.Errorf("%s decoded without error to %v", bad, a)
		}
	}

	for _, tc := range []struct {
		in   Claims
		want string
	}{
		{Claims{Audience: Audience{"api"}}, `{"aud":"api"}`},
		{Claims{Audience: Audience{"api", "web"}}, `{"aud":["api","web"]}`},
		{Claims{}, `{}`},
	} {
		raw, err := json.Marshal(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != tc.want {
			t.Errorf("Marshal(%+v) = %s, want %s", tc.in, raw, tc.want)
		}
	}
}

// The header typ is not part of the claim set, so it never rides the payload.
func TestClaims_TypeIsNotEncoded(t *testing.T) {
	raw, err := json.Marshal(Claims{Issuer: "i", Type: TypeAccessToken})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"iss":"i"}` {
		t.Errorf("Marshal = %s, want only iss", raw)
	}
}

func TestValidate(t *testing.T) {
	now := time.Unix(1_760_000_000, 500_000_000)
	sec := now.Unix()
	good := Claims{Issuer: "iss", Audience: Audience{"api"}, Expiry: sec + 60, NotBefore: sec - 60, IssuedAt: sec - 60, Type: TypeJWT}
	want := Want{Issuer: "iss", Audience: "api"}
	with := func(f func(*Claims)) Claims {
		c := good
		f(&c)
		return c
	}

	for _, tc := range []struct {
		name string
		c    Claims
		want Want
		err  error
	}{
		{"all good", good, want, nil},
		{"wrong issuer", with(func(c *Claims) { c.Issuer = "other" }), want, ErrIssuer},
		{"issuer not asked for", with(func(c *Claims) { c.Issuer = "other" }), Want{Audience: "api"}, nil},
		{"audience in an array", with(func(c *Claims) { c.Audience = Audience{"web", "api"} }), want, nil},
		{"wrong audience", with(func(c *Claims) { c.Audience = Audience{"web"} }), want, ErrAudience},
		{"no audience when one is wanted", with(func(c *Claims) { c.Audience = nil }), want, ErrAudience},
		{"an audience when none is wanted", good, Want{Issuer: "iss"}, ErrAudience},
		{"no audience, none wanted", with(func(c *Claims) { c.Audience = nil }), Want{Issuer: "iss"}, nil},
		{"typ matches", good, Want{Issuer: "iss", Audience: "api", Type: "jwt"}, nil},
		{"typ differs", good, Want{Issuer: "iss", Audience: "api", Type: TypeAccessToken}, ErrType},
		{"no exp", with(func(c *Claims) { c.Expiry = 0 }), want, ErrExpired},
		{"exp is now", with(func(c *Claims) { c.Expiry = sec }), want, ErrExpired},
		{"exp just passed", with(func(c *Claims) { c.Expiry = sec - 1 }), want, ErrExpired},
		{"exp passed within leeway", with(func(c *Claims) { c.Expiry = sec - 1 }), Want{Issuer: "iss", Audience: "api", Leeway: 5 * time.Second}, nil},
		{"negative leeway is zero", with(func(c *Claims) { c.Expiry = sec }), Want{Issuer: "iss", Audience: "api", Leeway: -time.Hour}, ErrExpired},
		{"nbf ahead", with(func(c *Claims) { c.NotBefore = sec + 10 }), want, ErrNotYetValid},
		{"nbf ahead within leeway", with(func(c *Claims) { c.NotBefore = sec + 10 }), Want{Issuer: "iss", Audience: "api", Leeway: 10 * time.Second}, nil},
		{"iat ahead", with(func(c *Claims) { c.IssuedAt = sec + 10 }), want, ErrIssuedInFuture},
		{"iat ahead within leeway", with(func(c *Claims) { c.IssuedAt = sec + 10 }), Want{Issuer: "iss", Audience: "api", Leeway: 10 * time.Second}, nil},
		{"nbf and iat absent", with(func(c *Claims) { c.NotBefore, c.IssuedAt = 0, 0 }), want, nil},
		{"issuer is checked before expiry", with(func(c *Claims) { c.Issuer, c.Expiry = "other", 1 }), want, ErrIssuer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate(tc.c, now, tc.want); !errors.Is(err, tc.err) || (tc.err == nil && err != nil) {
				t.Errorf("Validate = %v, want %v", err, tc.err)
			}
		})
	}
}

// A verifier whose request carries no audience at all is the case the
// default exists for: Want's zero value must refuse an audience-bound token.
func TestValidate_ZeroWantRefusesAnAudience(t *testing.T) {
	now := time.Now()
	c := Claims{Audience: Audience{"someone-else"}, Expiry: now.Add(time.Minute).Unix()}
	if err := Validate(c, now, Want{}); !errors.Is(err, ErrAudience) {
		t.Fatalf("Validate = %v, want ErrAudience", err)
	}
}
