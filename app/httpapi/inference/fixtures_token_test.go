package inference

import (
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/pkg/crypto"
)

// mintToken signs, under priv and kid, the claims a mint issues to userID in
// projectID: version 1, a fresh jti, an hour to live. mutate, when set,
// edits the claims before signing.
func mintToken(t testing.TB, priv ed25519.PrivateKey, kid, userID, projectID string, mutate func(*crypto.TokenClaims)) string {
	t.Helper()
	claims := crypto.TokenClaims{
		Iss: crypto.TokenIssuer,
		Sub: "user:" + userID,
		Prj: projectID,
		Ver: 1,
		Jti: meta.NewID(),
		Iat: time.Now().Unix(),
		Exp: time.Now().Add(time.Hour).Unix(),
	}
	if mutate != nil {
		mutate(&claims)
	}
	tok, err := crypto.SignToken(priv, kid, claims)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return tok
}
