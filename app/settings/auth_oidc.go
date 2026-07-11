package settings

import (
	"fmt"
	"net/url"
	"os"
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

	// PostLoginURL is where the callback sends the browser after minting
	// the session. Empty means "/" — correct when the admin UI is embedded
	// (same origin as the callback). Set it to the UI's origin when the UI
	// is served from a different hostname than the control API, so a login
	// doesn't strand the user on the API origin.
	PostLoginURL string `json:"postLoginUrl,omitempty"`
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

// AuthOIDCEnv reads the WYOLET_* environment overlay. WYOLET_AUTH_MODE=oidc
// returns a fully-formed enabled section that short-circuits the DB-backed
// one — deployment env is the platform's config channel, and turning SSO on
// must not require a live DB write. Any other mode value (including the
// platform-contract "insecure-dev", which relay covers with password login)
// returns (nil, nil): overlay inactive. A malformed active overlay returns
// an error; callers on the boot path should treat it as fatal.
func AuthOIDCEnv() (*AuthOIDC, error) {
	if os.Getenv("WYOLET_AUTH_MODE") != "oidc" {
		return nil, nil
	}
	c := &AuthOIDC{
		Enabled:         true,
		Issuer:          os.Getenv("WYOLET_OIDC_ISSUER"),
		ClientID:        os.Getenv("WYOLET_OIDC_CLIENT_ID"),
		ClientSecretEnv: "WYOLET_OIDC_CLIENT_SECRET",
		RedirectURL:     os.Getenv("WYOLET_OIDC_REDIRECT_URL"),
		Registration:    os.Getenv("WYOLET_OIDC_REGISTRATION"),
		PostLoginURL:    os.Getenv("WYOLET_OIDC_POST_LOGIN_URL"),
	}
	if c.Registration == "" {
		c.Registration = "open"
	}
	if os.Getenv(c.ClientSecretEnv) == "" {
		return nil, fmt.Errorf("WYOLET_AUTH_MODE=oidc: WYOLET_OIDC_CLIENT_SECRET is not set")
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("WYOLET_AUTH_MODE=oidc: %w (set the matching WYOLET_OIDC_* var)", err)
	}
	return c, nil
}

// EffectiveAuthOIDC is what handlers obey: the env overlay when active, else
// the DB-backed section. A malformed overlay resolves to the DB path here —
// the boot path has already fatal'd on it, so a running process never
// carries one.
func EffectiveAuthOIDC(r Reader) *AuthOIDC {
	if c, err := AuthOIDCEnv(); err == nil && c != nil {
		return c
	}
	return AuthOIDCFrom(r)
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
