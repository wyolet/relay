package settings

import (
	"testing"

	"github.com/wyolet/relay/app/settings/settingstest"
)

// fakeChecker unlocks exactly the features it lists.
type fakeChecker map[string]bool

func (f fakeChecker) Has(feature string) bool { return f[feature] }

// grantLicense installs a gate unlocking features for the duration of the
// test. With no arguments the deployment is community.
func grantLicense(t *testing.T, features ...string) {
	t.Helper()
	f := fakeChecker{}
	for _, name := range features {
		f[name] = true
	}
	SetLicenseGate(f)
	t.Cleanup(func() { gate.Store(nil) })
}

// enabledOIDC is a well-formed, enabled auth:oidc value.
const enabledOIDC = `{"enabled":true,"issuer":"https://idp.example.com",` +
	`"clientId":"c1","redirectUrl":"https://relay.example.com/auth/callback"}`

// OIDC login is community: an upgrade must not lock out a deployment whose
// users only sign in through their IdP.
func TestOIDCLoginNeedsNoLicense(t *testing.T) {
	grantLicense(t)
	sec, ok := Lookup(AuthOIDCSection)
	if !ok {
		t.Fatal("auth:oidc not registered")
	}
	v, err := sec.Decode([]byte(enabledOIDC))
	if err != nil {
		t.Fatalf("unlicensed decode: %v", err)
	}
	if c, _ := v.(*AuthOIDC); c == nil || !c.Enabled {
		t.Fatalf("decode = %+v, want the enabled section", v)
	}

	setOIDCEnvVars(t)
	c, err := AuthOIDCEnv()
	if err != nil || c == nil || !c.Enabled {
		t.Fatalf("unlicensed overlay = %+v, %v; want it active", c, err)
	}
	if got := EffectiveAuthOIDC(nil); !got.Enabled {
		t.Fatalf("effective config = %+v, want the overlay", got)
	}
}

// A malformed stored section still fails the read; only a license gate
// degrades.
func TestStoredOIDCStillValidates(t *testing.T) {
	grantLicense(t)
	sec, _ := Lookup(AuthOIDCSection)
	if _, err := decodeOrDegrade(sec, []byte(`{"enabled":`)); err == nil {
		t.Fatal("malformed JSON must still fail the read")
	}
	if _, err := decodeOrDegrade(sec, []byte(`{"enabled":true}`)); err == nil {
		t.Fatal("an incomplete enabled section must still fail validation")
	}
}

func TestLicenseSectionRoundTrip(t *testing.T) {
	sec, ok := Lookup(SectionLicense)
	if !ok {
		t.Fatal("license section not registered")
	}
	v, err := sec.Decode([]byte(`{"value":"a.b.c"}`))
	if err != nil {
		t.Fatal(err)
	}
	if l, _ := v.(*License); l == nil || l.Value != "a.b.c" {
		t.Fatalf("decode = %+v", v)
	}
	if got := LicenseFrom(nil).Value; got != "" {
		t.Errorf("nil reader = %q, want the community zero value", got)
	}
	if got := LicenseFrom(settingstest.Sections(map[string]any{SectionLicense: &License{Value: "x.y.z"}})).Value; got != "x.y.z" {
		t.Errorf("LicenseFrom = %q", got)
	}
}
