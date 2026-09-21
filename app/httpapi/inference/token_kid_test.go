package inference

import (
	"crypto/ed25519"
	"net/http"
	"testing"
	"time"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/pkg/crypto"
)

// signedWith mints a token for the fixture's user under priv, naming kid.
func signedWith(t *testing.T, f principalFixture, priv ed25519.PrivateKey, kid string) string {
	t.Helper()
	tok, err := crypto.SignToken(priv, kid, crypto.TokenClaims{
		Iss: crypto.TokenIssuer, Sub: "user:" + f.user, Prj: f.project.Meta.ID,
		Ver: 1, Jti: meta.NewID(), Iat: time.Now().Unix(),
		Exp: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return tok
}

func TestSigningKeyRotationKeepsThePreviousKeyLive(t *testing.T) {
	f := newPrincipalFixture()
	f.bindings = []*policybinding.PolicyBinding{
		boundTo(f, "bind-all", 10, f.boundPol.Meta.ID, "group:system:authenticated"),
	}
	oldPub := f.signer.Public().(ed25519.PublicKey)
	old := signedWith(t, f, f.signer, crypto.KeyID(oldPub))

	newPub, newPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	f.tokens.SetKey(newPub)
	fresh := signedWith(t, f, newPriv, crypto.KeyID(newPub))

	st := f.stack(t)
	if w := st.do(fresh); w.Code != http.StatusOK {
		t.Fatalf("token under the new key: status %d: %s", w.Code, w.Body)
	}
	if w := st.do(old); w.Code != http.StatusOK {
		t.Fatalf("token under the previous key: status %d: %s — rotation must not be a global logout", w.Code, w.Body)
	}

	// A second rotation retires the original key.
	thirdPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	f.tokens.SetKey(thirdPub)
	if w := st.do(old); w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 once the key is two rotations old", w.Code)
	}
}

// Tokens minted before the kid header existed carry none and must keep
// verifying against the current key through the transition.
func TestTokenWithoutKidVerifiesAgainstTheCurrentKey(t *testing.T) {
	f := newPrincipalFixture()
	f.bindings = []*policybinding.PolicyBinding{
		boundTo(f, "bind-all", 10, f.boundPol.Meta.ID, "group:system:authenticated"),
	}
	bare := signedWith(t, f, f.signer, "")
	if crypto.TokenKeyID(bare) != "" {
		t.Fatal("expected a token with no kid")
	}
	if w := f.stack(t).do(bare); w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
}

func TestUnknownKidIsRefused(t *testing.T) {
	f := newPrincipalFixture()
	tok := signedWith(t, f, f.signer, "deadbeefdeadbeef")
	if w := f.stack(t).do(tok); w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a kid naming no held key", w.Code)
	}
}
