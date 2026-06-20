package oauth

import "golang.org/x/oauth2"

// ProviderConfig is a serializable description of an OAuth provider. It is the
// shape loaded from a config file — relay seeds it as a `oauth:<provider>`
// settings section (operator-overridable, not the catalog), and an SDK consumer
// can unmarshal its own YAML/JSON into it. Vendor specifics (client id,
// endpoints, scopes) are config data, never values baked into this module, so a
// provider rotating its client id or endpoints is a config edit, not a release.
type ProviderConfig struct {
	ClientID string `json:"clientId" yaml:"clientId"`

	// Issuer is the provider's OAuth 2.0 issuer URL. When set, Discover fetches
	// its authorization-server metadata (RFC 8414) to fill any endpoint left
	// empty below. Optional — leave it empty and set the endpoints explicitly if
	// the provider publishes no discovery document.
	Issuer string `json:"issuer,omitempty" yaml:"issuer,omitempty"`

	AuthURL       string `json:"authUrl"                 yaml:"authUrl"`
	TokenURL      string `json:"tokenUrl"                yaml:"tokenUrl"`
	DeviceAuthURL string `json:"deviceAuthUrl,omitempty" yaml:"deviceAuthUrl,omitempty"`
	RedirectURI   string `json:"redirectUri,omitempty"   yaml:"redirectUri,omitempty"`

	Scopes []string `json:"scopes,omitempty" yaml:"scopes,omitempty"`

	// AuthParams are extra authorization-URL query params (e.g. {"code":"true"}
	// for Anthropic's hosted copy/paste callback). Surfaced via AuthCodeOptions.
	AuthParams map[string]string `json:"authParams,omitempty" yaml:"authParams,omitempty"`
}

// OAuth2 builds the x/oauth2 config. AuthStyle is forced to in-params: these are
// public PKCE clients that carry client_id in the request, with no client
// secret to send via Basic auth.
func (c ProviderConfig) OAuth2() *oauth2.Config {
	return &oauth2.Config{
		ClientID:    c.ClientID,
		RedirectURL: c.RedirectURI,
		Scopes:      c.Scopes,
		Endpoint: oauth2.Endpoint{
			AuthURL:       c.AuthURL,
			TokenURL:      c.TokenURL,
			DeviceAuthURL: c.DeviceAuthURL,
			AuthStyle:     oauth2.AuthStyleInParams,
		},
	}
}

// AuthCodeOptions converts AuthParams into authorize-URL options for
// (*Flow).AuthorizeURL.
func (c ProviderConfig) AuthCodeOptions() []oauth2.AuthCodeOption {
	opts := make([]oauth2.AuthCodeOption, 0, len(c.AuthParams))
	for k, v := range c.AuthParams {
		opts = append(opts, oauth2.SetAuthURLParam(k, v))
	}
	return opts
}
