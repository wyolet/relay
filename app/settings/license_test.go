package settings

import (
	"errors"
	"testing"

	"github.com/wyolet/relay/app/license"
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

func TestAuthOIDCDecodeRequiresLicense(t *testing.T) {
	sec, ok := Lookup(AuthOIDCSection)
	if !ok {
		t.Fatal("auth:oidc not registered")
	}

	t.Run("enabled without a license", func(t *testing.T) {
		grantLicense(t)
		v, err := sec.Decode([]byte(enabledOIDC))
		if !errors.Is(err, license.ErrRequired) {
			t.Fatalf("decode = %+v, err = %v, want license_required", v, err)
		}
	})

	t.Run("enabled with an sso license", func(t *testing.T) {
		grantLicense(t, license.FeatureSSO)
		v, err := sec.Decode([]byte(enabledOIDC))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if c, _ := v.(*AuthOIDC); c == nil || !c.Enabled {
			t.Fatalf("decode = %+v, want the enabled section", v)
		}
	})

	t.Run("disabled needs no license", func(t *testing.T) {
		grantLicense(t)
		if _, err := sec.Decode([]byte(`{"enabled":false}`)); err != nil {
			t.Fatalf("a disabled section must decode unlicensed: %v", err)
		}
	})
}

func TestAuthOIDCEnvRefusedWithoutLicense(t *testing.T) {
	grantLicense(t)
	setOIDCEnvVars(t)

	c, err := AuthOIDCEnv()
	if !errors.Is(err, license.ErrRequired) {
		t.Fatalf("overlay = %+v, err = %v, want license_required", c, err)
	}
	// The refusal must not promote the overlay: password login stands.
	if got := EffectiveAuthOIDC(nil); got.Enabled {
		t.Fatalf("effective config = %+v, want disabled", got)
	}
}

// A deployment that stored an enabled section and then lost its license must
// keep booting with SSO off, not fail every settings read.
func TestStoredOIDCDegradesWithoutLicense(t *testing.T) {
	grantLicense(t)
	sec, _ := Lookup(AuthOIDCSection)

	v, err := decodeOrDegrade(sec, []byte(enabledOIDC))
	if err != nil {
		t.Fatalf("an unlicensed stored section must not fail the read: %v", err)
	}
	if c, _ := v.(*AuthOIDC); c == nil || c.Enabled {
		t.Fatalf("degraded value = %+v, want the disabled default", v)
	}

	// Only the license gate degrades — a malformed row is still an error.
	if _, err := decodeOrDegrade(sec, []byte(`{"enabled":`)); err == nil {
		t.Fatal("malformed JSON must still fail the read")
	}
	grantLicense(t, license.FeatureSSO)
	if _, err := decodeOrDegrade(sec, []byte(`{"enabled":true}`)); err == nil {
		t.Fatal("a licensed but incomplete section must still fail validation")
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
	if got := LicenseFrom(licenseReader{v: &License{Value: "x.y.z"}}).Value; got != "x.y.z" {
		t.Errorf("LicenseFrom = %q", got)
	}
}

type licenseReader struct{ v *License }

func (r licenseReader) Setting(section string) (any, bool) {
	if section == SectionLicense {
		return r.v, true
	}
	return nil, false
}
