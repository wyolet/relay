package settings

import (
	"fmt"
	"net/url"
)

// AuthOIDC is the auth:oidc settings section: inbound OpenID Connect login
// for the control plane. Disabled by default — the zero value changes
// nothing about a deployment. When enabled, GET /auth/oidc/start redirects
// to the provider and /auth/oidc/callback exchanges the code and mints a
// normal relay session, so everything downstream of login is unchanged.
//
// The provider is generic OIDC: any issuer publishing a discovery document
// (RFC 8414 / OpenID discovery) works. The client secret is referenced by
// env var name, never stored in the section value.
type AuthOIDC struct {
	Enabled bool `json:"enabled"`

	// Issuer is the provider's issuer URL; endpoints are discovered from
	// its metadata document.
	Issuer   string `json:"issuer,omitempty"`
	ClientID string `json:"clientId,omitempty"`

	// ClientSecretEnv names the env var holding the client secret.
	// Indirection keeps the secret out of the settings row (world-readable
	// to any authenticated operator).
	ClientSecretEnv string `json:"clientSecretEnv,omitempty"`

	// RedirectURL is this relay's public callback,
	// e.g. https://relay.example.com/api/auth/oidc/callback.
	RedirectURL string `json:"redirectUrl,omitempty"`

	// Scopes defaults to ["openid", "profile", "email"].
	Scopes []string `json:"scopes,omitempty"`

	// AuthParams are extra authorization-URL query params some providers
	// require beyond the OIDC standard set (e.g. WorkOS AuthKit needs
	// {"provider": "authkit"}). Provider-specific values live here in
	// config, never in code.
	AuthParams map[string]string `json:"authParams,omitempty"`

	// Registration gates first-login auto-provisioning: "open" creates a
	// user row on first successful OIDC login; "closed" (default) rejects
	// subjects with no existing user row.
	Registration string `json:"registration,omitempty"`
}

// EffectiveScopes returns Scopes or the OIDC default set.
func (c *AuthOIDC) EffectiveScopes() []string {
	if len(c.Scopes) > 0 {
		return c.Scopes
	}
	return []string{"openid", "profile", "email"}
}

// OpenRegistration reports whether first-login auto-provisioning is on.
func (c *AuthOIDC) OpenRegistration() bool { return c.Registration == "open" }

// Validate enforces shape only when enabled — a disabled section may be
// sparse or empty.
func (c *AuthOIDC) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.Issuer == "" {
		return fmt.Errorf("auth:oidc: issuer is required when enabled")
	}
	if _, err := url.Parse(c.Issuer); err != nil {
		return fmt.Errorf("auth:oidc: issuer: %w", err)
	}
	if c.ClientID == "" {
		return fmt.Errorf("auth:oidc: clientId is required when enabled")
	}
	if c.RedirectURL == "" {
		return fmt.Errorf("auth:oidc: redirectUrl is required when enabled")
	}
	switch c.Registration {
	case "", "open", "closed":
	default:
		return fmt.Errorf("auth:oidc: registration must be \"open\" or \"closed\"")
	}
	return nil
}

// AuthOIDCSection is the section name.
const AuthOIDCSection = "auth:oidc"

func init() {
	Register(Section{
		Name: AuthOIDCSection,
		Description: "Inbound OpenID Connect login for the control plane. " +
			"Generic OIDC (issuer discovery + authorization-code flow); disabled by default. " +
			"registration=open auto-provisions a user on first login.",
		Defaults: func() any { return &AuthOIDC{} },
		Decode:   decodeAndValidate[AuthOIDC, *AuthOIDC],
	})
}

// AuthOIDCFrom reads the typed section from a settings Reader, tolerating
// absent or mistyped values (returns the zero value → disabled).
func AuthOIDCFrom(r Reader) *AuthOIDC {
	if r == nil {
		return &AuthOIDC{}
	}
	if v, ok := r.Setting(AuthOIDCSection); ok {
		if c, ok := v.(*AuthOIDC); ok {
			return c
		}
	}
	return &AuthOIDC{}
}
