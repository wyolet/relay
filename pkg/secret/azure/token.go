package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	vaultScope       = "https://vault.azure.net/.default"
	vaultResource    = "https://vault.azure.net"
	tokenRefreshSkew = 60 * time.Second
	imdsAPIVersion   = "2019-08-01"
	defaultIMDS      = "http://169.254.169.254/metadata/identity/oauth2/token"
)

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
}

type tokenError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func (r *Resolver) accessToken(ctx context.Context) (string, error) {
	r.tokenMu.Lock()
	defer r.tokenMu.Unlock()

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

func (r *Resolver) fetchClientCredentialsToken(ctx context.Context) (string, int, error) {
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
		"scope":         {vaultScope},
	}

	return r.postToken(ctx, tokenURL, form.Encode(), "application/x-www-form-urlencoded", nil)
}

func (r *Resolver) fetchManagedIdentityToken(ctx context.Context) (string, int, error) {
	tokenURL := r.cfg.IMDSEndpoint
	if tokenURL == "" {
		u, err := url.Parse(defaultIMDS)
		if err != nil {
			return "", 0, fmt.Errorf("secret/azure: parse imds url: %w", err)
		}
		q := u.Query()
		q.Set("api-version", imdsAPIVersion)
		q.Set("resource", vaultResource)
		u.RawQuery = q.Encode()
		tokenURL = u.String()
	}

	return r.requestToken(ctx, http.MethodGet, tokenURL, "", "", map[string]string{"Metadata": "true"})
}

func (r *Resolver) postToken(ctx context.Context, tokenURL, body, contentType string, extraHeaders map[string]string) (string, int, error) {
	return r.requestToken(ctx, http.MethodPost, tokenURL, body, contentType, extraHeaders)
}

func (r *Resolver) requestToken(ctx context.Context, method, tokenURL, body, contentType string, extraHeaders map[string]string) (string, int, error) {
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
