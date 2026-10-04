package control

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wyolet/relay/app/user"
)

func configFeatures(t *testing.T, d Deps) map[string]bool {
	t.Helper()
	w := httptest.NewRecorder()
	ConfigJSONHandler(d)(w, httptest.NewRequest(http.MethodGet, "/config.json", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body RuntimeConfig
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body.Features
}

func TestConfigJSONAdvertisesTheWiredFeatures(t *testing.T) {
	off := configFeatures(t, Deps{})
	for _, name := range []string{"tokens", "users", "audit"} {
		v, ok := off[name]
		if !ok {
			t.Errorf("feature %q is absent; the UI cannot tell it off from unknown", name)
		}
		if v {
			t.Errorf("feature %q is true on an unwired deployment", name)
		}
	}
	if _, ok := off["oidc"]; ok {
		t.Error("oidc is advertised without the setting enabled")
	}

	signer := &TokenSigner{}
	seed := make([]byte, ed25519.SeedSize)
	signer.SetSeed(seed)
	on := configFeatures(t, Deps{
		TokenSigner: signer,
		Users:       &user.Store{},
		AuditReader: staticAudit{},
	})
	for _, name := range []string{"tokens", "users", "audit"} {
		if !on[name] {
			t.Errorf("feature %q is false though it is wired", name)
		}
	}
}
