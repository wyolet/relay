// Package azure resolves secret.KindAzure refs by fetching from Azure Key Vault.
// Auth uses Entra ID client-credentials (or optional managed-identity IMDS) and
// only the Go standard library. Fetch-only: plaintext is held in memory, never
// written to Postgres.
package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/wyolet/relay/pkg/secret"
)

const (
	apiVersion       = "2025-07-01"
	vaultScope       = "https://vault.azure.net/.default"
	vaultResource    = "https://vault.azure.net"
	defaultClient    = 30 * time.Second
	tokenRefreshSkew = 60 * time.Second
	imdsAPIVersion   = "2019-08-01"
	defaultIMDS      = "http://169.254.169.254/metadata/identity/oauth2/token"
)

// Config holds Key Vault and Entra settings. Secret fields must be supplied by
// the composition layer from env — never hard-coded.
type Config struct {
	VaultURL           string
	TenantID           string
	ClientID           string
	ClientSecret       string
	UseManagedIdentity bool
	HTTPClient         *http.Client
	// TokenEndpoint overrides the Entra token URL (tests).
	TokenEndpoint string
	// IMDSEndpoint overrides the managed-identity token URL (tests).
	IMDSEndpoint string
	Now          func() time.Time
}

// ConfigFromEnv reads AZURE_KEYVAULT_URL and, unless AZURE_USE_MANAGED_IDENTITY=1,
// AZURE_TENANT_ID, AZURE_CLIENT_ID, and AZURE_CLIENT_SECRET.
func ConfigFromEnv() (Config, error) {
	vault := strings.TrimSpace(os.Getenv("AZURE_KEYVAULT_URL"))
	if vault == "" {
		return Config{}, fmt.Errorf("secret/azure: AZURE_KEYVAULT_URL is unset or empty")
	}
	if os.Getenv("AZURE_USE_MANAGED_IDENTITY") == "1" {
		return Config{VaultURL: vault, UseManagedIdentity: true}, nil
	}
	tenant := strings.TrimSpace(os.Getenv("AZURE_TENANT_ID"))
	clientID := strings.TrimSpace(os.Getenv("AZURE_CLIENT_ID"))
	clientSecret := strings.TrimSpace(os.Getenv("AZURE_CLIENT_SECRET"))
	if tenant == "" {
		return Config{}, fmt.Errorf("secret/azure: AZURE_TENANT_ID is required")
	}
	if clientID == "" || clientSecret == "" {
		return Config{}, fmt.Errorf("secret/azure: AZURE_CLIENT_ID and AZURE_CLIENT_SECRET are required")
	}
	return Config{
		VaultURL:     vault,
		TenantID:     tenant,
		ClientID:     clientID,
		ClientSecret: clientSecret,
	}, nil
}

// Resolver fetches secrets from Azure Key Vault (KindAzure only).
type Resolver struct {
	cfg Config

	tokenMu     sync.Mutex
	cachedToken string
	cachedExp   time.Time
}

var _ secret.Resolver = (*Resolver)(nil)

// New returns a Resolver for secret.KindAzure refs.
func New(cfg Config) *Resolver {
	return &Resolver{cfg: cfg}
}

// Resolve returns the secret value for ref.Path = "<secretName>[/<version>]".
// The vault URL comes from Config.VaultURL.
func (r *Resolver) Resolve(ctx context.Context, ref secret.Ref) ([]byte, error) {
	if ref.Kind != secret.KindAzure {
		return nil, fmt.Errorf("secret/azure: wrong kind %q", ref.Kind)
	}
	name, version, err := parsePath(ref.Path)
	if err != nil {
		return nil, err
	}
	token, err := r.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	return r.getSecret(ctx, name, version, token)
}

func parsePath(path string) (name, version string, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", "", fmt.Errorf("secret/azure: path is empty")
	}
	name = path
	if i := strings.Index(path, "/"); i >= 0 {
		name = path[:i]
		version = path[i+1:]
		if name == "" {
			return "", "", fmt.Errorf("secret/azure: empty secret name in path %q", path)
		}
		if version == "" {
			return "", "", fmt.Errorf("secret/azure: empty version in path %q", path)
		}
	}
	return name, version, nil
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

type secretBundle struct {
	Value string `json:"value"`
}

type keyVaultError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
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

func (r *Resolver) getSecret(ctx context.Context, name, version, token string) ([]byte, error) {
	base := strings.TrimSuffix(strings.TrimSpace(r.cfg.VaultURL), "/")
	if base == "" {
		return nil, fmt.Errorf("secret/azure: vault URL is not configured")
	}

	seg := url.PathEscape(name)
	path := base + "/secrets/" + seg
	if version != "" {
		path += "/" + url.PathEscape(version)
	}
	u, err := url.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("secret/azure: parse secret url: %w", err)
	}
	q := u.Query()
	q.Set("api-version", apiVersion)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("secret/azure: build secret request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := r.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("secret/azure: secret request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("secret/azure: read secret response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, keyVaultErrorFromBody(resp.StatusCode, respBody)
	}

	var bundle secretBundle
	if err := json.Unmarshal(respBody, &bundle); err != nil {
		return nil, fmt.Errorf("secret/azure: decode secret response: %w", err)
	}
	if bundle.Value == "" {
		return nil, fmt.Errorf("secret/azure: secret %q has empty value", name)
	}
	return []byte(bundle.Value), nil
}

func (r *Resolver) httpClient() *http.Client {
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

func keyVaultErrorFromBody(status int, body []byte) error {
	var e keyVaultError
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		if e.Error.Code != "" {
			return fmt.Errorf("secret/azure: %s: %s (HTTP %d)", e.Error.Code, e.Error.Message, status)
		}
		return fmt.Errorf("secret/azure: %s (HTTP %d)", e.Error.Message, status)
	}
	return fmt.Errorf("secret/azure: key vault returned HTTP %d", status)
}
