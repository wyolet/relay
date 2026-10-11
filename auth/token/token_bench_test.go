package token

import (
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
