package control

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/user"
)

// runtimeConfig reads /config.json from the listener root and from the API route, which must answer the same body and cache header.
func runtimeConfig(t *testing.T, d Deps) RuntimeConfig {
	t.Helper()
	const cache = "public, max-age=60, stale-while-revalidate=600"
	root := httptest.NewRecorder()
	ConfigJSONHandler(d)(root, httptest.NewRequest(http.MethodGet, "/config.json", nil))
	r, api := testAPI(t, nil)
	registerConfigJSON(api, d)
	route := scopeReq(t, r, "", http.MethodGet, "/config.json", "")
	var fromRoot, fromRoute RuntimeConfig
	wantJSON(t, root, http.StatusOK, &fromRoot)
	wantJSON(t, route, http.StatusOK, &fromRoute)
	if diff := cmp.Diff(fromRoot, fromRoute); diff != "" {
		t.Errorf("root and API route differ (-root +route):\n%s", diff)
	}
	if got := root.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("root Content-Type = %q", got)
	}
	if a, b := root.Header().Get("Cache-Control"), route.Header().Get("Cache-Control"); a != cache || b != cache {
		t.Errorf("Cache-Control = %q and %q, want %q", a, b, cache)
	}
	return fromRoot
}

// tokens, users, audit and accessReview are always present, false included: the UI cannot tell a missing flag from an unknown one. oidc appears only when enabled.
func TestConfigJSON(t *testing.T) {
	// The env overlay would otherwise advertise oidc from the developer's own environment.
	t.Setenv("WYOLET_AUTH_MODE", "")
	signer := &TokenSigner{}
	signer.SetSeed(make([]byte, ed25519.SeedSize))
	static := RuntimeConfig{
		ControlAPIURL: "https://relay.test/api", InferenceAPIURL: "https://api.relay.test", Mode: "cloud", Version: "1.2.3",
		Features: map[string]bool{"beta": true, "tokens": true}, Telemetry: &Telemetry{SentryDSN: "https://dsn.test/1", Environment: "prod"},
		DocsURL: "https://docs.test", SupportURL: "https://support.test",
	}
	wired := static
	wired.Features = map[string]bool{"beta": true, "tokens": true, "users": true, "audit": true, "accessReview": true}
	wired.PseudoKinds = []string{"agents"}
	off := map[string]bool{"tokens": false, "users": false, "audit": false, "accessReview": false}

	for _, tc := range []struct {
		name string
		d    Deps
		want RuntimeConfig
	}{
		{"nothing wired", Deps{}, RuntimeConfig{Version: httpapi.Version, Features: off, PseudoKinds: []string{}}},
		{"a signer without a key mints nothing", Deps{TokenSigner: &TokenSigner{}, RuntimeConfig: RuntimeConfig{Features: map[string]bool{"tokens": true}}},
			RuntimeConfig{Version: httpapi.Version, Features: off, PseudoKinds: []string{}}},
		{"everything wired", Deps{RuntimeConfig: static, TokenSigner: signer, Users: &user.Store{}, AuditReader: staticAudit{}, Authz: audit.Authorizer{Inner: authz.RBAC{}}, PseudoKinds: []string{"agents"}}, wired},
		{"every pseudo kind, a name no kind has left out", Deps{PseudoKinds: []string{"all", "gadgets"}}, RuntimeConfig{Version: httpapi.Version, Features: off, PseudoKinds: []string{"agents"}}},
		{"only names no kind has", Deps{PseudoKinds: []string{"gadgets"}}, RuntimeConfig{Version: httpapi.Version, Features: off, PseudoKinds: []string{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, runtimeConfig(t, tc.d)); diff != "" {
				t.Errorf("config (-want +got):\n%s", diff)
			}
		})
	}
	if diff := cmp.Diff(map[string]bool{"beta": true, "tokens": true}, static.Features); diff != "" {
		t.Errorf("the static features map was written to (-want +got):\n%s", diff)
	}
}

// The access review answers only under an authorizer that explains; the audit wrapper delegates Explain either way, so the flag must look through it.
func TestConfigJSONAccessReviewFollowsTheAuthorizer(t *testing.T) {
	t.Setenv("WYOLET_AUTH_MODE", "")
	for name, tc := range map[string]struct {
		az   authz.Authorizer
		want bool
	}{
		"single":         {authz.AlwaysAllowAuthenticated{}, false},
		"single audited": {audit.Authorizer{Inner: authz.AlwaysAllowAuthenticated{}}, false},
		"rbac":           {authz.RBAC{}, true},
		"rbac audited":   {audit.Authorizer{Inner: authz.RBAC{}}, true},
	} {
		if got := runtimeConfig(t, Deps{Authz: tc.az}).Features["accessReview"]; got != tc.want {
			t.Errorf("%s: accessReview = %v, want %v", name, got, tc.want)
		}
	}
}
