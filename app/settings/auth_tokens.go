package settings

import (
	"fmt"
	"time"

	"github.com/wyolet/relay/pkg/secret"
)

// AuthTokens is the auth:tokens settings section: short-lived, relay-signed
// inference tokens a logged-in user mints for a project. Enabled by default,
// but a deployment with no master key has nowhere to keep the signing key and
// stays token-less until one is configured.
type AuthTokens struct {
	Enabled bool `json:"enabled"`

	// DefaultTTL applies when a mint request names no ttl; MaxTTL caps what
	// a request may ask for. Both are time.Duration — nanoseconds on the
	// wire, as everywhere Go durations are serialized.
	DefaultTTL time.Duration `json:"defaultTTL"`
	MaxTTL     time.Duration `json:"maxTTL"`

	// SigningKey points at the stored Ed25519 seed. Empty means "not
	// generated yet": boot mints one, stores it through the master-key path,
	// and writes the ref back here.
	SigningKey secret.Ref `json:"signingKey,omitempty"`
}

// Validate is enforced before any write.
func (a *AuthTokens) Validate() error {
	if a.DefaultTTL <= 0 || a.MaxTTL <= 0 {
		return fmt.Errorf("auth:tokens: defaultTTL and maxTTL must be > 0")
	}
	if a.DefaultTTL > a.MaxTTL {
		return fmt.Errorf("auth:tokens: defaultTTL must not exceed maxTTL")
	}
	if a.SigningKey.Kind != "" {
		if a.SigningKey.Kind != secret.KindStored {
			return fmt.Errorf("auth:tokens: signingKey must be a stored secret ref")
		}
		if err := a.SigningKey.Validate(); err != nil {
			return fmt.Errorf("auth:tokens: signingKey: %w", err)
		}
	}
	return nil
}

// AuthTokensSection is the section name.
const AuthTokensSection = "auth:tokens"

func defaultAuthTokens() *AuthTokens {
	return &AuthTokens{Enabled: true, DefaultTTL: time.Hour, MaxTTL: 24 * time.Hour}
}

func init() {
	Register(Section{
		Name: AuthTokensSection,
		Description: "Relay-signed inference tokens (EdDSA JWTs) a logged-in user mints for a project. " +
			"defaultTTL applies when a mint request names none, maxTTL caps what it may ask for " +
			"(both in nanoseconds). signingKey is filled in at boot with a generated key stored " +
			"under the master key; without a master key tokens stay disabled.",
		Defaults: func() any { return defaultAuthTokens() },
		Decode:   decodeAndValidate[AuthTokens, *AuthTokens],
	})
}

// AuthTokensFrom reads the typed section from a settings Reader, tolerating
// absent or mistyped values (returns the defaults).
func AuthTokensFrom(r Reader) *AuthTokens {
	if r == nil {
		return defaultAuthTokens()
	}
	if v, ok := r.Setting(AuthTokensSection); ok {
		if c, ok := v.(*AuthTokens); ok {
			return c
		}
	}
	return defaultAuthTokens()
}
