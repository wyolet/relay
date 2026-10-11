package oidc

import (
	"context"
	"errors"
	"fmt"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
)

// discoveryTTL bounds how long a provider's metadata and signing keys are reused before its metadata is fetched again.
const discoveryTTL = time.Hour

// cachedProvider holds the discovered endpoints and the provider's JWKS cache; the oauth2 config is rebuilt per login so setting edits apply on the next one.
type cachedProvider struct {
	provider *gooidc.Provider
	expires  time.Time
}

// provider returns cfg's discovered provider, fetched from the issuer's OpenID discovery document at most once per discoveryTTL for each issuer, client and redirect URL.
func (rp *RelyingParty[U]) provider(ctx context.Context, cfg *Config) (*gooidc.Provider, error) {
	key := cfg.Issuer + "|" + cfg.ClientID + "|" + cfg.RedirectURL
	rp.mu.Lock()
	if e, ok := rp.discovery[key]; ok && time.Now().Before(e.expires) {
		rp.mu.Unlock()
		return e.provider, nil
	}
	rp.mu.Unlock()

	p, err := discover(ctx, cfg.Issuer)
	if err != nil {
		return nil, err
	}
	rp.mu.Lock()
	if rp.discovery == nil {
		rp.discovery = map[string]cachedProvider{}
	}
	rp.discovery[key] = cachedProvider{provider: p, expires: time.Now().Add(discoveryTTL)}
	rp.mu.Unlock()
	return p, nil
}

// discover fetches the issuer's OpenID discovery document; go-oidc refuses one whose issuer differs from the configured issuer, the value every id_token's iss must then equal.
func discover(ctx context.Context, issuer string) (*gooidc.Provider, error) {
	if issuer == "" {
		return nil, errors.New("oidc: discovery needs an issuer")
	}
	p, err := gooidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc: discover %s: %w", issuer, err)
	}
	if ep := p.Endpoint(); ep.AuthURL == "" || ep.TokenURL == "" {
		return nil, fmt.Errorf("oidc: discover %s: metadata missing authorization_endpoint or token_endpoint", issuer)
	}
	return p, nil
}
