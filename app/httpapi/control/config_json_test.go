package control

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
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
	// The env overlay would otherwise advertise oidc from the developer's own environment.
	t.Setenv("WYOLET_AUTH_MODE", "")
	alwaysPresent := []string{"tokens", "users", "audit", "accessReview"}

	off := configFeatures(t, Deps{})
	for _, name := range alwaysPresent {
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
		Authz:       audit.Authorizer{Inner: authz.RBAC{}},
	})
	for _, name := range alwaysPresent {
		if !on[name] {
			t.Errorf("feature %q is false though it is wired", name)
		}
	}
}

// The access review answers only under an authorizer that explains; the audit wrapper delegates Explain either way, so the flag must look through it.
func TestConfigJSONAccessReviewFollowsTheAuthorizer(t *testing.T) {
	for name, tc := range map[string]struct {
		az   authz.Authorizer
		want bool
	}{
		"single":         {authz.AlwaysAllowAuthenticated{}, false},
		"single audited": {audit.Authorizer{Inner: authz.AlwaysAllowAuthenticated{}}, false},
		"rbac":           {authz.RBAC{}, true},
		"rbac audited":   {audit.Authorizer{Inner: authz.RBAC{}}, true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := configFeatures(t, Deps{Authz: tc.az})["accessReview"]; got != tc.want {
				t.Errorf("accessReview = %v, want %v", got, tc.want)
			}
		})
	}
}
