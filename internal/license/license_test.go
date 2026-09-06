package license

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	applicense "github.com/wyolet/relay/app/license"
)

// newTestKey installs a freshly generated release key for the duration of
// the test. Keys are never committed — every run signs with its own pair.
func newTestKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	prev := publicKey
	publicKey = base64.StdEncoding.EncodeToString(pub)
	t.Cleanup(func() { publicKey = prev })
	return priv
}

func sign(t *testing.T, priv ed25519.PrivateKey, c claims) string {
	t.Helper()
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA","typ":"JWT"}`))
	body, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	payload := base64.RawURLEncoding.EncodeToString(body)
	sig := ed25519.Sign(priv, []byte(hdr+"."+payload))
	return hdr + "." + payload + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// validClaims is a license good for a year from now.
func validClaims(now time.Time) claims {
	return claims{
		Issuer:      issuer,
		Subject:     "acme",
		IssuedAt:    now.Add(-time.Hour).Unix(),
		Expires:     now.Add(365 * 24 * time.Hour).Unix(),
		Deployments: 1,
		Features:    []string{applicense.FeatureSSO, "scim"},
		Support:     "business",
		ID:          "lic-1",
	}
}

func TestParseValidLicenseFeatureTruthTable(t *testing.T) {
	priv := newTestKey(t)
	now := time.Now()
	l, err := Parse(sign(t, priv, validClaims(now)))
	if err != nil {
		t.Fatal(err)
	}
	if l.Customer != "acme" || l.Deployments != 1 || l.Support != "business" {
		t.Fatalf("claims not carried through: %+v", l)
	}
	if l.Grace {
		t.Error("an unexpired license must not report grace")
	}
	for feature, want := range map[string]bool{
		applicense.FeatureSSO: true,
		"scim":                true,
		"custom-roles":        false,
		"orgs":                false,
		"":                    false,
	} {
		if got := l.Has(feature); got != want {
			t.Errorf("Has(%q) = %v, want %v", feature, got, want)
		}
	}
	var nilLicense *License
	if nilLicense.Has(applicense.FeatureSSO) {
		t.Error("a nil license must gate everything")
	}
}

func TestParseRejectsUntrustedLicenses(t *testing.T) {
	priv := newTestKey(t)
	now := time.Now()

	wrongIssuer := validClaims(now)
	wrongIssuer.Issuer = "not-wyolet"

	notYetValid := validClaims(now)
	notYetValid.IssuedAt = now.Add(48 * time.Hour).Unix()

	// Same signature, a payload claiming a longer feature list.
	extra := validClaims(now)
	extra.Features = append(extra.Features, "orgs")
	tampered := sign(t, priv, validClaims(now))
	tampered = strings.Join([]string{
		strings.Split(tampered, ".")[0],
		strings.Split(sign(t, priv, extra), ".")[1],
		strings.Split(tampered, ".")[2],
	}, ".")

	cases := map[string]string{
		"wrong issuer":  sign(t, priv, wrongIssuer),
		"not yet valid": sign(t, priv, notYetValid),
		"not a jwt":     "garbage",
		"tampered":      tampered,
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			if l, err := Parse(value); err == nil {
				t.Fatalf("want rejection, got %+v", l)
			}
		})
	}

	t.Run("signed by another key", func(t *testing.T) {
		_, foreign, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if l, err := Parse(sign(t, foreign, validClaims(now))); err == nil {
			t.Fatalf("want rejection, got %+v", l)
		}
	})

	// Every rejection is community, not a crash: the Service keeps serving.
	svc := New(nil)
	if _, err := svc.Set("garbage"); err == nil {
		t.Fatal("Set must report an unusable value")
	}
	if svc.Has(applicense.FeatureSSO) || svc.Info().Licensed {
		t.Fatal("a rejected license must leave the service unlicensed")
	}
}

func TestExpiryGraceThenCommunity(t *testing.T) {
	priv := newTestKey(t)
	issued := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expires := issued.Add(24 * time.Hour)
	c := validClaims(issued)
	c.IssuedAt = issued.Unix()
	c.Expires = expires.Unix()
	value := sign(t, priv, c)

	for _, tc := range []struct {
		name          string
		at            time.Time
		wantLicensed  bool
		wantGrace     bool
		wantSSOAccess bool
	}{
		{"before expiry", expires.Add(-time.Hour), true, false, true},
		{"one day into grace", expires.Add(24 * time.Hour), true, true, true},
		{"last hour of grace", expires.Add(GraceWindow - time.Hour), true, true, true},
		{"past grace", expires.Add(GraceWindow + time.Hour), false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := New(func() time.Time { return tc.at })
			info, err := svc.Set(value)
			if tc.wantLicensed && err != nil {
				t.Fatalf("Set: %v", err)
			}
			if !tc.wantLicensed && err == nil {
				t.Fatal("want a past-grace rejection")
			}
			if info.Licensed != tc.wantLicensed || info.Grace != tc.wantGrace {
				t.Errorf("info = %+v, want licensed=%v grace=%v", info, tc.wantLicensed, tc.wantGrace)
			}
			if got := svc.Has(applicense.FeatureSSO); got != tc.wantSSOAccess {
				t.Errorf("Has(sso) = %v, want %v", got, tc.wantSSOAccess)
			}
		})
	}
}

func TestSetKeepsPreviousLicenseOnBadValue(t *testing.T) {
	priv := newTestKey(t)
	svc := New(nil)
	if _, err := svc.Set(sign(t, priv, validClaims(time.Now()))); err != nil {
		t.Fatal(err)
	}
	before := svc.Info()
	if _, err := svc.Set("not-a-license"); err == nil {
		t.Fatal("want an error for an unusable value")
	}
	got := svc.Info()
	if got.Customer != before.Customer || !got.Licensed || !svc.Has(applicense.FeatureSSO) {
		t.Fatalf("previous license must survive: %+v, want %+v", got, before)
	}
}

func TestLoadEnvWinsOverStoredValue(t *testing.T) {
	priv := newTestKey(t)
	now := time.Now()

	envClaims := validClaims(now)
	envClaims.Subject = "from-env"
	storedClaims := validClaims(now)
	storedClaims.Subject = "from-settings"

	t.Run("no environment", func(t *testing.T) {
		t.Setenv("RELAY_LICENSE", "")
		t.Setenv("RELAY_LICENSE_FILE", "")
		svc := New(nil)
		info, err := svc.Set(sign(t, priv, storedClaims))
		if err != nil || info.Customer != "from-settings" {
			t.Fatalf("info = %+v, err = %v", info, err)
		}
	})

	t.Run("inline environment", func(t *testing.T) {
		t.Setenv("RELAY_LICENSE", sign(t, priv, envClaims))
		svc := New(nil)
		info, err := svc.Set(sign(t, priv, storedClaims))
		if err != nil || info.Customer != "from-env" {
			t.Fatalf("info = %+v, err = %v", info, err)
		}
	})

	t.Run("file environment", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "relay.lic")
		if err := os.WriteFile(path, []byte(sign(t, priv, envClaims)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("RELAY_LICENSE", "")
		t.Setenv("RELAY_LICENSE_FILE", path)
		l, err := Load(context.Background())
		if err != nil || l == nil || l.Customer != "from-env" {
			t.Fatalf("Load = %+v, err = %v", l, err)
		}
	})

	t.Run("unset environment is community, not an error", func(t *testing.T) {
		t.Setenv("RELAY_LICENSE", "")
		t.Setenv("RELAY_LICENSE_FILE", "")
		l, err := Load(context.Background())
		if err != nil || l != nil {
			t.Fatalf("Load = %+v, err = %v", l, err)
		}
	})
}
