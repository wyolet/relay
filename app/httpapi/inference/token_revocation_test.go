package inference

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/wyolet/relay/pkg/crypto"
)

// Signing out everywhere (a token-version bump) answers the same code as a
// per-jti revocation, so a client has one thing to key on.
func TestTokenVersionRevocationAnswersTokenRevoked(t *testing.T) {
	f := newPrincipalFixture()
	token := f.mint(t, func(c *crypto.TokenClaims) { c.Ver = 0 }) // stale version
	w := f.stack(t).do(token)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", w.Code, w.Body)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != "token_revoked" {
		t.Errorf("code = %q, want token_revoked", body.Error.Code)
	}
}
