package crypto

import (
	"crypto/ed25519"
	"testing"
	"time"
)

func benchClaims() TokenClaims {
	return TokenClaims{
		Iss: TokenIssuer, Sub: "user:019200aa", Prj: "019200bb",
		Grp: []string{"platform-eng", "data-science"},
		Ver: 3, Jti: "019200cc",
		Iat: time.Now().Unix(), Exp: time.Now().Add(time.Hour).Unix(),
	}
}

func BenchmarkSignToken(b *testing.B) {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		b.Fatal(err)
	}
	claims := benchClaims()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := SignToken(priv, "0011223344556677", claims); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseToken(b *testing.B) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		b.Fatal(err)
	}
	token, err := SignToken(priv, KeyID(pub), benchClaims())
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ParseToken(pub, token); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTokenKeyID(b *testing.B) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		b.Fatal(err)
	}
	token, err := SignToken(priv, KeyID(pub), benchClaims())
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = TokenKeyID(token)
	}
}
