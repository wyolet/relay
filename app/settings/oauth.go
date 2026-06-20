package settings

import (
	"fmt"

	"github.com/wyolet/relay/sdk/oauth"
)

// OAuthProvider is the typed value of an oauth:<provider> settings section: the
// flow config (authorize/token endpoints, client id, scopes, authorize params)
// used to acquire and refresh that provider's OAuth credentials.
//
// The relay core ships only this generic shape. Concrete provider values are
// operator/community config — register a provider's section with
// RegisterOAuthProvider and supply the values via the settings API or a
// config/settings/oauth-<provider>.yaml. No provider is registered by default.
type OAuthProvider struct {
	oauth.ProviderConfig
}

// Validate enforces the minimum a flow + refresh needs.
func (c *OAuthProvider) Validate() error {
	if c.ClientID == "" {
		return fmt.Errorf("oauth: clientId is required")
	}
	if c.AuthURL == "" {
		return fmt.Errorf("oauth: authUrl is required")
	}
	if c.TokenURL == "" {
		return fmt.Errorf("oauth: tokenUrl is required")
	}
	return nil
}

// OAuthSectionPrefix namespaces per-provider OAuth sections (oauth:anthropic,
// oauth:openai, …), mirroring the governance:<kind> convention.
const OAuthSectionPrefix = "oauth:"

// OAuthSection returns the settings section name for a provider.
func OAuthSection(provider string) string { return OAuthSectionPrefix + provider }

// RegisterOAuthProvider registers the oauth:<provider> settings section so its
// config can be seeded and edited via the settings API. Call once per provider
// (e.g. from a vendor wiring file in the composition root) before settings are
// seeded or served. Panics on duplicate registration.
func RegisterOAuthProvider(provider string) {
	Register(Section{
		Name:        OAuthSection(provider),
		Description: "OAuth provider config for " + provider + " (authorize/token endpoints, client id, scopes, authorize params). Drives the SDK login flow and server-side token refresh.",
		Defaults:    func() any { return &OAuthProvider{} },
		Decode:      decodeAndValidate[OAuthProvider, *OAuthProvider],
	})
}
