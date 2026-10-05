package control

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/wyolet/relay/app/settings"
)

// oidcFlowSecret writes an auth:oidc body through the section's Decode (the
// PUT path) and, when accepted, drives the anonymous start + callback.
// It returns the client_secret the issuer's token endpoint received.
func oidcFlowSecret(t *testing.T, idp *fakeIdP, secretEnv string) (string, error) {
	t.Helper()
	sec, _ := settings.Lookup(settings.AuthOIDCSection)
	v, err := sec.Decode([]byte(`{"enabled":true,"issuer":"` + idp.srv.URL + `","clientId":"client-1","clientSecretEnv":"` + secretEnv +
		`","redirectUrl":"http://relay.test/api/auth/oidc/callback","registration":"open"}`))
	if err != nil {
		return "", err
	}
	cfg := v.(*settings.AuthOIDC)
	od := &oidcDeps{
		cfg:      func() *settings.AuthOIDC { return cfg },
		users:    newFakeUsers(),
		sessions: &fakeSessions{},
		discover: cachedDiscover(),
	}
	loc, flow := driveStart(t, od)
	cb := httptest.NewRequest("GET", "/api/auth/oidc/callback?code="+idp.issuedCode+"&state="+url.QueryEscape(loc.Query().Get("state")), nil)
	cb.AddCookie(flow)
	w := httptest.NewRecorder()
	od.callback(w, cb)
	if w.Code != http.StatusFound {
		t.Fatalf("callback: status %d body %s", w.Code, w.Body.String())
	}
	return idp.sawSecret, nil
}

// An auth:oidc writer must not be able to name a relay credential as the
// client secret and have it posted to an issuer of their choosing.
func TestOIDCClientSecretEnvOutsideScopeNeverReachesIssuer(t *testing.T) {
	const dummy = "dummy-admin-token-value"
	t.Setenv("RELAY_ADMIN_TOKEN", dummy)
	t.Setenv("RELAY_COOKIE_SECURE", "false") // the fake issuer is plain http

	got, err := oidcFlowSecret(t, newFakeIdP(t), "RELAY_ADMIN_TOKEN")
	if got == dummy {
		t.Fatalf("token endpoint received the value of RELAY_ADMIN_TOKEN")
	}
	if err == nil {
		t.Fatal("auth:oidc accepted RELAY_ADMIN_TOKEN as clientSecretEnv")
	}
}

func TestOIDCClientSecretEnvInScopeReachesIssuer(t *testing.T) {
	t.Setenv("RELAY_OIDC_CLIENT_SECRET", "oidc-secret")
	t.Setenv("RELAY_COOKIE_SECURE", "false")
	got, err := oidcFlowSecret(t, newFakeIdP(t), "RELAY_OIDC_CLIENT_SECRET")
	if err != nil {
		t.Fatal(err)
	}
	if got != "oidc-secret" {
		t.Fatalf("token endpoint saw client_secret=%q", got)
	}
}

// Without the plain-HTTP opt-out an http issuer is refused outright.
func TestOIDCPlainHTTPIssuerRefused(t *testing.T) {
	t.Setenv("RELAY_OIDC_CLIENT_SECRET", "oidc-secret")
	t.Setenv("RELAY_COOKIE_SECURE", "")
	idp := newFakeIdP(t)
	if _, err := oidcFlowSecret(t, idp, "RELAY_OIDC_CLIENT_SECRET"); err == nil {
		t.Fatal("auth:oidc accepted a plain-http issuer")
	}
	if idp.sawSecret != "" {
		t.Fatal("secret was sent")
	}
}
