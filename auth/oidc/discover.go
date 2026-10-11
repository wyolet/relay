package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// discoveryTTL bounds how long a provider's endpoints are reused before its metadata is fetched again.
const discoveryTTL = time.Hour

type cachedEndpoint struct {
	authURL, tokenURL string
	expires           time.Time
}

// serverMetadata is the subset of RFC 8414 / OpenID discovery metadata the login flow uses.
type serverMetadata struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
}

// endpoints returns cfg's authorization and token endpoints, fetched from the issuer's metadata at most once per discoveryTTL for each issuer, client and redirect URL.
func (rp *RelyingParty[U]) endpoints(ctx context.Context, cfg *Config) (cachedEndpoint, error) {
	key := cfg.Issuer + "|" + cfg.ClientID + "|" + cfg.RedirectURL
	rp.mu.Lock()
	if e, ok := rp.discovery[key]; ok && time.Now().Before(e.expires) {
		rp.mu.Unlock()
		return e, nil
	}
	rp.mu.Unlock()

	md, err := discover(ctx, cfg.Issuer)
	if err != nil {
		return cachedEndpoint{}, err
	}
	e := cachedEndpoint{authURL: md.AuthorizationEndpoint, tokenURL: md.TokenEndpoint, expires: time.Now().Add(discoveryTTL)}
	rp.mu.Lock()
	if rp.discovery == nil {
		rp.discovery = map[string]cachedEndpoint{}
	}
	rp.discovery[key] = e
	rp.mu.Unlock()
	return e, nil
}

// discover fetches the issuer's metadata, RFC 8414 first and then OpenID discovery: providers commonly publish only one of the two.
func discover(ctx context.Context, issuer string) (serverMetadata, error) {
	if issuer == "" {
		return serverMetadata{}, errors.New("oidc: discovery needs an issuer")
	}
	base := strings.TrimRight(issuer, "/")
	var (
		md      serverMetadata
		lastErr error
	)
	for _, url := range []string{
		base + "/.well-known/oauth-authorization-server",
		base + "/.well-known/openid-configuration",
	} {
		md, lastErr = fetchMetadata(ctx, url)
		if lastErr == nil {
			break
		}
	}
	if lastErr != nil {
		return serverMetadata{}, lastErr
	}
	if md.AuthorizationEndpoint == "" || md.TokenEndpoint == "" {
		return serverMetadata{}, fmt.Errorf("oidc: discover %s: metadata missing authorization_endpoint or token_endpoint", issuer)
	}
	return md, nil
}

func fetchMetadata(ctx context.Context, url string) (serverMetadata, error) {
	var md serverMetadata
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return md, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return md, fmt.Errorf("oidc: discover %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return md, fmt.Errorf("oidc: discover %s: status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&md); err != nil {
		return md, fmt.Errorf("oidc: discover %s: decode: %w", url, err)
	}
	return md, nil
}
