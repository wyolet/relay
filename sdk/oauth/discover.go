package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// asMetadata is the subset of RFC 8414 Authorization Server Metadata we consume.
type asMetadata struct {
	AuthorizationEndpoint       string `json:"authorization_endpoint"`
	TokenEndpoint               string `json:"token_endpoint"`
	DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
}

// Discover fills any endpoint the caller left empty (AuthURL, TokenURL,
// DeviceAuthURL) from the provider's OAuth 2.0 Authorization Server Metadata
// (RFC 8414) at Issuer + "/.well-known/oauth-authorization-server". Endpoints
// the caller already set are preserved — explicit config always wins. Client id,
// scopes, redirect URI, and authorize params are never touched (app intent, not
// server metadata).
//
// It is a no-op (no network call) when AuthURL and TokenURL are both already
// set. Otherwise Issuer must be set. hc is the HTTP client (nil →
// http.DefaultClient); the request honours ctx. Returns a new ProviderConfig;
// the receiver is unchanged.
func (c ProviderConfig) Discover(ctx context.Context, hc *http.Client) (ProviderConfig, error) {
	if c.AuthURL != "" && c.TokenURL != "" {
		return c, nil // core endpoints already configured; nothing to discover
	}
	if c.Issuer == "" {
		return c, fmt.Errorf("oauth: Discover needs Issuer (or set authUrl/tokenUrl explicitly)")
	}
	if hc == nil {
		hc = http.DefaultClient
	}

	// RFC 8414 first, then OpenID Connect discovery — providers commonly
	// publish only one of the two paths.
	base := strings.TrimRight(c.Issuer, "/")
	var (
		md      asMetadata
		lastErr error
	)
	for _, url := range []string{
		base + "/.well-known/oauth-authorization-server",
		base + "/.well-known/openid-configuration",
	} {
		m, err := fetchASMetadata(ctx, hc, url)
		if err != nil {
			lastErr = err
			continue
		}
		md = m
		lastErr = nil
		break
	}
	if lastErr != nil {
		return c, lastErr
	}

	out := c
	if out.AuthURL == "" {
		out.AuthURL = md.AuthorizationEndpoint
	}
	if out.TokenURL == "" {
		out.TokenURL = md.TokenEndpoint
	}
	if out.DeviceAuthURL == "" {
		out.DeviceAuthURL = md.DeviceAuthorizationEndpoint
	}
	if out.AuthURL == "" || out.TokenURL == "" {
		return c, fmt.Errorf("oauth: discover %s: metadata missing authorization_endpoint or token_endpoint", c.Issuer)
	}
	return out, nil
}

func fetchASMetadata(ctx context.Context, hc *http.Client, url string) (asMetadata, error) {
	var md asMetadata
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return md, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return md, fmt.Errorf("oauth: discover %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return md, fmt.Errorf("oauth: discover %s: status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&md); err != nil {
		return md, fmt.Errorf("oauth: discover %s: decode: %w", url, err)
	}
	return md, nil
}
