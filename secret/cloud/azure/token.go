// Package azure obtains Microsoft Entra ID access tokens for an Azure resource through client credentials or managed identity (IMDS), over the Go standard library (no Azure SDK), for any Azure API client in this module. Out of scope: workload identity federation, certificate credentials and calling any Azure service.
package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	tokenRefreshSkew = 60 * time.Second
	imdsAPIVersion   = "2019-08-01"
	defaultIMDS      = "http://169.254.169.254/metadata/identity/oauth2/token"
	defaultClient    = 30 * time.Second
)

// Config selects how a TokenSource authenticates and for which resource. Secret fields come from the caller's configuration, never hard-coded.
type Config struct {
	TenantID           string
	ClientID           string
	ClientSecret       string
	UseManagedIdentity bool
	// Resource is the Azure resource the token is for, e.g. "https://vault.azure.net"; client credentials request the "<Resource>/.default" scope.
	Resource string
	// HTTPClient is optional; defaults to a 30s timeout client.
	HTTPClient *http.Client
	// TokenEndpoint overrides the Entra token URL (tests).
	TokenEndpoint string
	// IMDSEndpoint overrides the managed-identity token URL (tests).
	IMDSEndpoint string
	Now          func() time.Time
}

// TokenSource returns a cached access token, fetching a new one a minute before the cached one expires. Safe for concurrent use.
type TokenSource struct {
	cfg Config

	mu          sync.Mutex
	cachedToken string
	cachedExp   time.Time
}

// NewTokenSource builds a TokenSource; nothing is fetched until AccessToken.
func NewTokenSource(cfg Config) *TokenSource {
	return &TokenSource{cfg: cfg}
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
}

type tokenError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// AccessToken returns a bearer token for the configured resource.
func (r *TokenSource) AccessToken(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now
	if r.cfg.Now != nil {
		now = r.cfg.Now
	}
	if r.cachedToken != "" && now().Before(r.cachedExp) {
		return r.cachedToken, nil
	}

	var (
		tok string
		exp int
		err error
	)
	if r.cfg.UseManagedIdentity {
		tok, exp, err = r.fetchManagedIdentityToken(ctx)
	} else {
		tok, exp, err = r.fetchClientCredentialsToken(ctx)
	}
	if err != nil {
		return "", err
	}

	ttl := time.Duration(exp) * time.Second
	if ttl > tokenRefreshSkew {
		ttl -= tokenRefreshSkew
	}
	r.cachedToken = tok
	r.cachedExp = now().Add(ttl)
	return tok, nil
}

func (r *TokenSource) fetchClientCredentialsToken(ctx context.Context) (string, int, error) {
	if strings.TrimSpace(r.cfg.TenantID) == "" {
		return "", 0, fmt.Errorf("secret/azure: tenant is not configured")
	}
	if r.cfg.ClientID == "" || r.cfg.ClientSecret == "" {
		return "", 0, fmt.Errorf("secret/azure: client credentials are not configured")
	}

	tokenURL := r.cfg.TokenEndpoint
	if tokenURL == "" {
		tokenURL = fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", r.cfg.TenantID)
	}

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {r.cfg.ClientID},
		"client_secret": {r.cfg.ClientSecret},
		"scope":         {r.cfg.Resource + "/.default"},
	}

	return r.postToken(ctx, tokenURL, form.Encode(), "application/x-www-form-urlencoded", nil)
}

func (r *TokenSource) fetchManagedIdentityToken(ctx context.Context) (string, int, error) {
	tokenURL := r.cfg.IMDSEndpoint
	if tokenURL == "" {
		u, err := url.Parse(defaultIMDS)
		if err != nil {
			return "", 0, fmt.Errorf("secret/azure: parse imds url: %w", err)
		}
		q := u.Query()
		q.Set("api-version", imdsAPIVersion)
		q.Set("resource", r.cfg.Resource)
		u.RawQuery = q.Encode()
		tokenURL = u.String()
	}

	return r.requestToken(ctx, http.MethodGet, tokenURL, "", "", map[string]string{"Metadata": "true"})
}

func (r *TokenSource) postToken(ctx context.Context, tokenURL, body, contentType string, extraHeaders map[string]string) (string, int, error) {
	return r.requestToken(ctx, http.MethodPost, tokenURL, body, contentType, extraHeaders)
}

func (r *TokenSource) requestToken(ctx context.Context, method, tokenURL, body, contentType string, extraHeaders map[string]string) (string, int, error) {
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, tokenURL, bodyReader)
	if err != nil {
		return "", 0, fmt.Errorf("secret/azure: build token request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := r.httpClient().Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("secret/azure: token request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", 0, fmt.Errorf("secret/azure: read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", 0, tokenErrorFromBody(resp.StatusCode, respBody)
	}

	var out tokenResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", 0, fmt.Errorf("secret/azure: decode token response: %w", err)
	}
	if out.AccessToken == "" {
		return "", 0, fmt.Errorf("secret/azure: token response missing access_token")
	}
	if out.ExpiresIn <= 0 {
		return "", 0, fmt.Errorf("secret/azure: token response missing expires_in")
	}
	return out.AccessToken, out.ExpiresIn, nil
}

func (r *TokenSource) httpClient() *http.Client {
	if r.cfg.HTTPClient != nil {
		return r.cfg.HTTPClient
	}
	return &http.Client{Timeout: defaultClient}
}

func tokenErrorFromBody(status int, body []byte) error {
	var e tokenError
	if json.Unmarshal(body, &e) == nil {
		msg := e.Error
		if e.ErrorDescription != "" {
			if msg != "" {
				msg += ": " + e.ErrorDescription
			} else {
				msg = e.ErrorDescription
			}
		}
		if msg != "" {
			return fmt.Errorf("secret/azure: token endpoint returned %q (HTTP %d)", msg, status)
		}
	}
	return fmt.Errorf("secret/azure: token endpoint returned HTTP %d", status)
}
