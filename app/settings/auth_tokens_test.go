package settings

import (
	"testing"
	"time"

	"github.com/wyolet/relay/app/settings/settingstest"
	"github.com/wyolet/relay/secret"
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
	stored := secret.Ref{Kind: secret.KindStored, ID: "auth-tokens-signing-key"}
	env := secret.Ref{Kind: secret.KindEnv, Env: "SIGNING_KEY"}
	for _, tc := range []struct {
		name    string
		mutate  func(*AuthTokens)
		wantErr string
	}{
		{"defaults", func(*AuthTokens) {}, ""},
		{"stored signing keys", func(a *AuthTokens) { a.SigningKey, a.PreviousSigningKey = stored, stored }, ""},
		{"default ttl at the max", func(a *AuthTokens) { a.DefaultTTL = a.MaxTTL }, ""},
		{"zero default ttl", func(a *AuthTokens) { a.DefaultTTL = 0 }, "auth:tokens: defaultTTL and maxTTL must be > 0"},
		{"negative max ttl", func(a *AuthTokens) { a.MaxTTL = -time.Second }, "auth:tokens: defaultTTL and maxTTL must be > 0"},
		{"default above max", func(a *AuthTokens) { a.DefaultTTL = a.MaxTTL + 1 }, "auth:tokens: defaultTTL must not exceed maxTTL"},
		{"env signing key", func(a *AuthTokens) { a.SigningKey = env }, "auth:tokens: signingKey must be a stored secret ref"},
		{"env previous signing key", func(a *AuthTokens) { a.PreviousSigningKey = env }, "auth:tokens: previousSigningKey must be a stored secret ref"},
		{"stored signing key with no id", func(a *AuthTokens) { a.SigningKey = secret.Ref{Kind: secret.KindStored} }, "auth:tokens: signingKey: " + noID},
		{"stored previous signing key with no id", func(a *AuthTokens) { a.PreviousSigningKey = secret.Ref{Kind: secret.KindStored} }, "auth:tokens: previousSigningKey: " + noID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid()
			tc.mutate(cfg)
			if got := errMsg(cfg.Validate()); got != tc.wantErr {
				t.Errorf("Validate = %q, want %q", got, tc.wantErr)
			}
		})
	}
}

// noID is the secret module's refusal of a stored ref that names nothing.
var noID = secret.Ref{Kind: secret.KindStored}.Validate().Error()

func errMsg(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestAuthTokens_KeepSigningKeys(t *testing.T) {
	cur := secret.Ref{Kind: secret.KindStored, ID: "auth-tokens-signing-key-b"}
	prev := secret.Ref{Kind: secret.KindStored, ID: "auth-tokens-signing-key-a"}
	installed := &AuthTokens{SigningKey: cur, PreviousSigningKey: prev}
	other := secret.Ref{Kind: secret.KindStored, ID: "elsewhere"}
	refusal := func(field string) string {
		return "auth:tokens: " + field + " is set by relay; rotate the key with POST /api/auth/token/keys/rotate"
	}
	for _, tc := range []struct {
		name     string
		next     AuthTokens
		current  *AuthTokens
		wantErr  string
		wantKeys [2]secret.Ref // signing, previous after the call
	}{
		{"omitted keeps installed", AuthTokens{}, installed, "", [2]secret.Ref{cur, prev}},
		{"echoed back", AuthTokens{SigningKey: cur, PreviousSigningKey: prev}, installed, "", [2]secret.Ref{cur, prev}},
		{"one echoed, one omitted", AuthTokens{PreviousSigningKey: prev}, installed, "", [2]secret.Ref{cur, prev}},
		{"nothing installed, nothing sent", AuthTokens{}, nil, "", [2]secret.Ref{}},
		{"other signing key", AuthTokens{SigningKey: other}, installed, refusal("signingKey"), [2]secret.Ref{other, {}}},
		{"other previous key", AuthTokens{PreviousSigningKey: other}, installed, refusal("previousSigningKey"), [2]secret.Ref{cur, other}},
		{"keys swapped", AuthTokens{SigningKey: prev, PreviousSigningKey: cur}, installed, refusal("signingKey"), [2]secret.Ref{prev, cur}},
		{"ref before any is installed", AuthTokens{SigningKey: cur}, nil, refusal("signingKey"), [2]secret.Ref{cur, {}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := tc.next
			if got := errMsg(next.KeepSigningKeys(tc.current)); got != tc.wantErr {
				t.Fatalf("KeepSigningKeys = %q, want %q", got, tc.wantErr)
			}
			if got := [2]secret.Ref{next.SigningKey, next.PreviousSigningKey}; got != tc.wantKeys {
				t.Errorf("refs = %+v, want %+v", got, tc.wantKeys)
			}
		})
	}
}

func TestAuthTokensFrom_MissingSection(t *testing.T) {
	// A reader that knows nothing must still answer with the defaults —
	// callers gate on Enabled, never on a nil.
	cfg := AuthTokensFrom(&settingstest.Source{})
	if cfg == nil || !cfg.Enabled || cfg.MaxTTL != 24*time.Hour {
		t.Errorf("AuthTokensFrom(empty) = %+v, want the defaults", cfg)
	}
	if cfg := AuthTokensFrom(nil); cfg == nil || !cfg.Enabled {
		t.Errorf("AuthTokensFrom(nil) = %+v, want the defaults", cfg)
	}
}
