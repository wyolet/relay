package token

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"
)

func benchClaims() testClaims {
	return testClaims{
		Iss: "relay", Sub: "user:019200aa", Prj: "019200bb",
		Grp: []string{"platform-eng", "data-science"},
		Ver: 3, Jti: "019200cc",
		Iat: time.Now().Unix(), Exp: time.Now().Add(time.Hour).Unix(),
	}
}

func BenchmarkSign(b *testing.B) {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		b.Fatal(err)
	}
	claims := benchClaims()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Sign(priv, "0011223344556677", claims); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParse(b *testing.B) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		b.Fatal(err)
	}
	token, err := Sign(priv, KeyID(pub), benchClaims())
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Parse[testClaims](pub, token); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkKeySetVerify is the hit path: kid lookup plus Parse, no store read.
func BenchmarkKeySetVerify(b *testing.B) {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		b.Fatal(err)
	}
	_, prev, err := ed25519.GenerateKey(nil)
	if err != nil {
		b.Fatal(err)
	}
	ks := NewKeySet(NewStaticStore(Keys{Signing: priv, Previous: []ed25519.PublicKey{prev.Public().(ed25519.PublicKey)}}), KeySetOptions{})
	if err := ks.Reload(context.Background()); err != nil {
		b.Fatal(err)
	}
	token, err := ks.Sign(benchClaims())
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Verify[testClaims](ctx, ks, token); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkValidate(b *testing.B) {
	now := time.Now()
	c := Claims{Issuer: "relay", Audience: Audience{"api"}, Expiry: now.Add(time.Hour).Unix(), IssuedAt: now.Unix()}
	want := Want{Issuer: "relay", Audience: "api", Leeway: time.Second}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := Validate(c, now, want); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHeaderKeyID(b *testing.B) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		b.Fatal(err)
	}
	token, err := Sign(priv, KeyID(pub), benchClaims())
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = HeaderKeyID(token)
	}
}
