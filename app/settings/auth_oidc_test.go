package settings

import (
	"strings"
	"testing"
)

// oidcReader serves a canned auth:oidc section value.
type oidcReader struct{ v *AuthOIDC }

func (f oidcReader) Setting(section string) (any, bool) {
	if section == AuthOIDCSection && f.v != nil {
		return f.v, true
	}
	return nil, false
}

func setOIDCEnv(t *testing.T) {
	t.Helper()
	t.Setenv("WYOLET_AUTH_MODE", "oidc")
	t.Setenv("WYOLET_OIDC_ISSUER", "https://idp.example.com")
	t.Setenv("WYOLET_OIDC_CLIENT_ID", "client-1")
	t.Setenv("WYOLET_OIDC_CLIENT_SECRET", "shh")
	t.Setenv("WYOLET_OIDC_REDIRECT_URL", "https://relay.example.com/auth/callback")
}

func TestAuthOIDCEnv_InactiveWhenModeUnset(t *testing.T) {
	t.Setenv("WYOLET_AUTH_MODE", "")
	t.Setenv("WYOLET_OIDC_ISSUER", "https://idp.example.com") // creds alone don't activate
	c, err := AuthOIDCEnv()
	if err != nil || c != nil {
		t.Fatalf("want inactive overlay, got %+v, %v", c, err)
	}
}

func TestAuthOIDCEnv_ActiveAndDefaultsOpen(t *testing.T) {
	setOIDCEnv(t)
	c, err := AuthOIDCEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !c.Enabled || c.Issuer != "https://idp.example.com" || c.ClientID != "client-1" {
		t.Errorf("overlay wrong: %+v", c)
	}
	if c.ClientSecretEnv != "WYOLET_OIDC_CLIENT_SECRET" {
		t.Errorf("ClientSecretEnv = %q", c.ClientSecretEnv)
	}
	if !c.OpenRegistration() {
		t.Error("registration should default open")
	}
}

func TestAuthOIDCEnv_PostLoginURL(t *testing.T) {
	setOIDCEnv(t)
	t.Setenv("WYOLET_OIDC_POST_LOGIN_URL", "https://ui.example.com/")
	c, err := AuthOIDCEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.PostLoginURL != "https://ui.example.com/" {
		t.Errorf("PostLoginURL = %q", c.PostLoginURL)
	}
}

func TestAuthOIDCEnv_RegistrationOverride(t *testing.T) {
	setOIDCEnv(t)
	t.Setenv("WYOLET_OIDC_REGISTRATION", "closed")
	c, err := AuthOIDCEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.OpenRegistration() {
		t.Error("registration override ignored")
	}
}

func TestAuthOIDCEnv_IncompleteFails(t *testing.T) {
	setOIDCEnv(t)
	t.Setenv("WYOLET_OIDC_REDIRECT_URL", "")
	if _, err := AuthOIDCEnv(); err == nil || !strings.Contains(err.Error(), "redirectUrl") {
		t.Fatalf("want redirectUrl error, got %v", err)
	}
	setOIDCEnv(t)
	t.Setenv("WYOLET_OIDC_CLIENT_SECRET", "")
	if _, err := AuthOIDCEnv(); err == nil || !strings.Contains(err.Error(), "WYOLET_OIDC_CLIENT_SECRET") {
		t.Fatalf("want client-secret error, got %v", err)
	}
}

func TestEffectiveAuthOIDC_EnvWinsOverDB(t *testing.T) {
	setOIDCEnv(t)
	db := oidcReader{v: &AuthOIDC{Enabled: true, Issuer: "https://db.example.com"}}
	if got := EffectiveAuthOIDC(db); got.Issuer != "https://idp.example.com" {
		t.Errorf("env overlay should win, got issuer %q", got.Issuer)
	}
}

func TestEffectiveAuthOIDC_FallsBackToDB(t *testing.T) {
	t.Setenv("WYOLET_AUTH_MODE", "")
	db := oidcReader{v: &AuthOIDC{Enabled: true, Issuer: "https://db.example.com"}}
	if got := EffectiveAuthOIDC(db); got.Issuer != "https://db.example.com" {
		t.Errorf("want DB section, got %+v", got)
	}
	if got := EffectiveAuthOIDC(nil); got.Enabled {
		t.Errorf("nil reader must resolve disabled, got %+v", got)
	}
}
