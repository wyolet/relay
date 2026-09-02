package settings

import (
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/secret"
)

func TestAuthTokens_Defaults(t *testing.T) {
	sec, ok := Lookup(AuthTokensSection)
	if !ok {
		t.Fatalf("section %q is not registered", AuthTokensSection)
	}
	cfg, _ := sec.Defaults().(*AuthTokens)
	if cfg == nil {
		t.Fatal("defaults are not *AuthTokens")
	}
	if !cfg.Enabled || cfg.DefaultTTL != time.Hour || cfg.MaxTTL != 24*time.Hour {
		t.Errorf("defaults = %+v, want enabled with 1h/24h", cfg)
	}
	if cfg.SigningKey.ID != "" {
		t.Errorf("defaults carry a signing key ref %+v, want none until boot generates one", cfg.SigningKey)
	}
}

func TestAuthTokens_Validate(t *testing.T) {
	valid := func() *AuthTokens {
		return &AuthTokens{Enabled: true, DefaultTTL: time.Hour, MaxTTL: 24 * time.Hour}
	}
	for _, tc := range []struct {
		name    string
		mutate  func(*AuthTokens)
		wantErr bool
	}{
		{name: "defaults", mutate: func(*AuthTokens) {}},
		{name: "stored signing key", mutate: func(a *AuthTokens) {
			a.SigningKey = secret.Ref{Kind: secret.KindStored, ID: "auth-tokens-signing-key"}
		}},
		{name: "zero ttl", mutate: func(a *AuthTokens) { a.DefaultTTL = 0 }, wantErr: true},
		{name: "default above max", mutate: func(a *AuthTokens) { a.DefaultTTL = 48 * time.Hour }, wantErr: true},
		{name: "env signing key", mutate: func(a *AuthTokens) {
			a.SigningKey = secret.Ref{Kind: secret.KindEnv, Env: "SIGNING_KEY"}
		}, wantErr: true},
		{name: "stored signing key with no id", mutate: func(a *AuthTokens) {
			a.SigningKey = secret.Ref{Kind: secret.KindStored}
		}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.mutate(cfg)
			if err := cfg.Validate(); (err != nil) != tc.wantErr {
				t.Errorf("Validate = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestAuthTokensFrom_MissingSection(t *testing.T) {
	// A reader that knows nothing must still answer with the defaults —
	// callers gate on Enabled, never on a nil.
	cfg := AuthTokensFrom(fakeReader{})
	if cfg == nil || !cfg.Enabled || cfg.MaxTTL != 24*time.Hour {
		t.Errorf("AuthTokensFrom(empty) = %+v, want the defaults", cfg)
	}
	if cfg := AuthTokensFrom(nil); cfg == nil || !cfg.Enabled {
		t.Errorf("AuthTokensFrom(nil) = %+v, want the defaults", cfg)
	}
}
