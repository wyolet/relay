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

// discover fetches the issuer's OpenID discovery document. Its issuer must equal the configured one or differ from it only by a trailing slash, and every id_token's iss must then equal the document's issuer exactly.
func discover(ctx context.Context, issuer string) (*gooidc.Provider, error) {
	if issuer == "" {
		return nil, errors.New("oidc: discovery needs an issuer")
	}
	p, err := gooidc.NewProvider(ctx, issuer)
	if mismatch, ok := errors.AsType[*gooidc.IssuerMismatchError](err); ok && slashOnly(issuer, mismatch.Discovered) {
		p, err = providerAt(ctx, issuer, mismatch.Discovered)
	}
	if err != nil {
		return nil, fmt.Errorf("oidc: discover %s: %w", issuer, err)
	}
	if ep := p.Endpoint(); ep.AuthURL == "" || ep.TokenURL == "" {
		return nil, fmt.Errorf("oidc: discover %s: metadata missing authorization_endpoint or token_endpoint", issuer)
	}
	return p, nil
}

// slashOnly reports whether a and b differ by exactly one trailing slash.
func slashOnly(a, b string) bool {
	return a+"/" == b || a == b+"/"
}

// providerAt fetches the document again with discovered as the issuer id_tokens must carry. go-oidc then skips its own issuer check, so the refetched document must still name discovered.
func providerAt(ctx context.Context, issuer, discovered string) (*gooidc.Provider, error) {
	p, err := gooidc.NewProvider(gooidc.InsecureIssuerURLContext(ctx, discovered), issuer)
	if err != nil {
		return nil, err
	}
	var md struct {
		Issuer string `json:"issuer"`
	}
	if err := p.Claims(&md); err != nil {
		return nil, err
	}
	if md.Issuer != discovered {
		return nil, &gooidc.IssuerMismatchError{Provided: discovered, Discovered: md.Issuer}
	}
	return p, nil
}
