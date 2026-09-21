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
	apiVersion    = "2025-07-01"
	defaultClient = 30 * time.Second
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

type secretBundle struct {
	Value string `json:"value"`
}

type keyVaultError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
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
