package token

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
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
		got := Audience{"stale"}
		if err := json.Unmarshal([]byte(tc.in), &got); err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if d := cmp.Diff(tc.want, got); d != "" {
			t.Errorf("%s decoded (-want +got):\n%s", tc.in, d)
		}
	}
	for _, bad := range []string{`1`, `{"a":1}`, `[1]`, `["api",1]`, `true`} {
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
		// The header typ is not part of the claim set, so it never rides the payload.
		{Claims{Issuer: "i", Type: TypeAccessToken}, `{"iss":"i"}`},
		{
			Claims{Issuer: "i", Subject: "s", Audience: Audience{"a"}, Expiry: 3, NotBefore: 1, IssuedAt: 2, ID: "j"},
			`{"iss":"i","sub":"s","aud":"a","exp":3,"nbf":1,"iat":2,"jti":"j"}`,
		},
	} {
		raw, err := json.Marshal(tc.in)
		must(t, err)
		if string(raw) != tc.want {
			t.Errorf("Marshal(%+v) = %s, want %s", tc.in, raw, tc.want)
		}
	}
}

func TestValidate(t *testing.T) {
	now := time.Unix(1_760_000_000, 500_000_000)
	sec := now.Unix()
	good := Claims{Issuer: "iss", Audience: Audience{"api"}, Expiry: sec + 60, NotBefore: sec - 60, IssuedAt: sec - 60, Type: TypeJWT}
	want := Want{Issuer: "iss", Audience: "api"}
	leeway := func(d time.Duration) Want { return Want{Issuer: "iss", Audience: "api", Leeway: d} }
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
		{"issuer differs in case", with(func(c *Claims) { c.Issuer = "ISS" }), want, ErrIssuer},
		{"issuer not asked for", with(func(c *Claims) { c.Issuer = "other" }), Want{Audience: "api"}, nil},
		{"audience in an array", with(func(c *Claims) { c.Audience = Audience{"web", "api"} }), want, nil},
		{"wrong audience", with(func(c *Claims) { c.Audience = Audience{"web"} }), want, ErrAudience},
		{"audience is a prefix of the wanted one", with(func(c *Claims) { c.Audience = Audience{"ap"} }), want, ErrAudience},
		{"no audience when one is wanted", with(func(c *Claims) { c.Audience = nil }), want, ErrAudience},
		{"an audience when none is wanted", good, Want{Issuer: "iss"}, ErrAudience},
		// Want's zero value must refuse a token bound to another service.
		{"an audience against the zero Want", good, Want{}, ErrAudience},
		{"no audience, none wanted", with(func(c *Claims) { c.Audience = nil }), Want{Issuer: "iss"}, nil},
		{"typ matches case-insensitively", good, Want{Issuer: "iss", Audience: "api", Type: "jwt"}, nil},
		{"typ differs", good, Want{Issuer: "iss", Audience: "api", Type: TypeAccessToken}, ErrType},
		{"typ wanted, none carried", with(func(c *Claims) { c.Type = "" }), Want{Issuer: "iss", Audience: "api", Type: TypeJWT}, ErrType},
		{"no exp", with(func(c *Claims) { c.Expiry = 0 }), want, ErrExpired},
		{"exp is now", with(func(c *Claims) { c.Expiry = sec }), want, ErrExpired},
		{"exp one second ahead", with(func(c *Claims) { c.Expiry = sec + 1 }), want, nil},
		{"exp just passed", with(func(c *Claims) { c.Expiry = sec - 1 }), want, ErrExpired},
		{"exp passed within leeway", with(func(c *Claims) { c.Expiry = sec - 4 }), leeway(5 * time.Second), nil},
		{"exp passed by the whole leeway", with(func(c *Claims) { c.Expiry = sec - 5 }), leeway(5 * time.Second), ErrExpired},
		{"negative leeway is zero", with(func(c *Claims) { c.Expiry = sec }), leeway(-time.Hour), ErrExpired},
		{"nbf ahead", with(func(c *Claims) { c.NotBefore = sec + 1 }), want, ErrNotYetValid},
		{"nbf ahead within leeway", with(func(c *Claims) { c.NotBefore = sec + 10 }), leeway(10 * time.Second), nil},
		{"nbf ahead past leeway", with(func(c *Claims) { c.NotBefore = sec + 11 }), leeway(10 * time.Second), ErrNotYetValid},
		{"iat ahead", with(func(c *Claims) { c.IssuedAt = sec + 1 }), want, ErrIssuedInFuture},
		{"iat ahead within leeway", with(func(c *Claims) { c.IssuedAt = sec + 10 }), leeway(10 * time.Second), nil},
		{"iat ahead past leeway", with(func(c *Claims) { c.IssuedAt = sec + 11 }), leeway(10 * time.Second), ErrIssuedInFuture},
		{"nbf and iat absent", with(func(c *Claims) { c.NotBefore, c.IssuedAt = 0, 0 }), want, nil},
		{"typ is checked before issuer", with(func(c *Claims) { c.Issuer = "other" }), Want{Issuer: "iss", Type: TypeAccessToken}, ErrType},
		{"issuer is checked before audience", with(func(c *Claims) { c.Issuer, c.Audience = "other", nil }), want, ErrIssuer},
		{"audience is checked before expiry", with(func(c *Claims) { c.Audience, c.Expiry = nil, 1 }), want, ErrAudience},
		{"expiry is checked before nbf", with(func(c *Claims) { c.Expiry, c.NotBefore = 1, sec+10 }), want, ErrExpired},
		{"nbf is checked before iat", with(func(c *Claims) { c.NotBefore, c.IssuedAt = sec+10, sec+10 }), want, ErrNotYetValid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate(tc.c, now, tc.want); err != tc.err {
				t.Errorf("Validate = %v, want %v", err, tc.err)
			}
		})
	}
}
