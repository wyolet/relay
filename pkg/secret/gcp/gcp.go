// Package gcp resolves secret.KindGCP refs by fetching from GCP Secret
// Manager. Service-account auth uses only the Go standard library (no Google
// SDK). Fetch-only: plaintext is held in memory, never written to Postgres.
package gcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/wyolet/relay/pkg/secret"
)

const (
	defaultSecretManagerHost = "https://secretmanager.googleapis.com"
	defaultClientTimeout     = 30 * time.Second
)

// Config holds GCP project and credentials for Secret Manager fetch-only
// resolution. ServiceAccountJSON and UseMetadataServer are mutually exclusive
// auth paths; when both are unset resolution errors at token fetch time.
type Config struct {
	ProjectID             string
	ServiceAccountJSON    []byte
	UseMetadataServer     bool
	HTTPClient            *http.Client
	TokenEndpoint         string
	SecretManagerEndpoint string
	MetadataEndpoint      string
	Now                   func() time.Time
}

// ConfigFromEnv reads GCP_PROJECT or GOOGLE_CLOUD_PROJECT, optional
// GCP_USE_METADATA_SERVER=1, and credentials from GCP_SA_JSON or
// GOOGLE_APPLICATION_CREDENTIALS. Returns an error when required config is
// missing.
func ConfigFromEnv() (Config, error) {
	project := strings.TrimSpace(os.Getenv("GCP_PROJECT"))
	if project == "" {
		project = strings.TrimSpace(os.Getenv("GOOGLE_CLOUD_PROJECT"))
	}

	useMetadata := os.Getenv("GCP_USE_METADATA_SERVER") == "1"
	if useMetadata {
		if project == "" {
			return Config{}, fmt.Errorf("secret/gcp: GCP_PROJECT is unset or empty")
		}
		return Config{
			ProjectID:         project,
			UseMetadataServer: true,
		}, nil
	}

	var saJSON []byte
	if inline := strings.TrimSpace(os.Getenv("GCP_SA_JSON")); inline != "" {
		saJSON = []byte(inline)
	} else if path := strings.TrimSpace(os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("secret/gcp: read GOOGLE_APPLICATION_CREDENTIALS: %w", err)
		}
		saJSON = raw
	}
	if len(saJSON) == 0 {
		return Config{}, fmt.Errorf("secret/gcp: GCP_SA_JSON or GOOGLE_APPLICATION_CREDENTIALS is required")
	}

	if project == "" {
		var doc struct {
			ProjectID string `json:"project_id"`
		}
		if err := json.Unmarshal(saJSON, &doc); err == nil && doc.ProjectID != "" {
			project = doc.ProjectID
		}
	}
	if project == "" {
		return Config{}, fmt.Errorf("secret/gcp: GCP_PROJECT is unset or empty")
	}

	return Config{
		ProjectID:          project,
		ServiceAccountJSON: saJSON,
	}, nil
}

// Resolver fetches secrets from GCP Secret Manager (KindGCP only).
type Resolver struct {
	cfg   Config
	token *tokenSource
}

var _ secret.Resolver = (*Resolver)(nil)

// New returns a Resolver for secret.KindGCP refs.
func New(cfg Config) *Resolver {
	ts := &tokenSource{cfg: cfg}
	if len(cfg.ServiceAccountJSON) > 0 {
		if sa, err := parseServiceAccountJSON(cfg.ServiceAccountJSON); err == nil {
			ts.sa = &sa
		}
	}
	return &Resolver{cfg: cfg, token: ts}
}

func (r *Resolver) Resolve(ctx context.Context, ref secret.Ref) ([]byte, error) {
	if ref.Kind != secret.KindGCP {
		return nil, fmt.Errorf("secret/gcp: wrong kind %q", ref.Kind)
	}
	secretName, version, err := parsePath(ref.Path)
	if err != nil {
		return nil, err
	}
	return r.accessSecret(ctx, secretName, version)
}

func parsePath(path string) (secretName, version string, err error) {
	if path == "" {
		return "", "", fmt.Errorf("secret/gcp: path is empty")
	}
	before, after, ok := strings.Cut(path, ":")
	if !ok {
		return path, "latest", nil
	}
	if before == "" {
		return "", "", fmt.Errorf("secret/gcp: path %q: missing secret name before %q", path, ":")
	}
	if after == "" {
		return "", "", fmt.Errorf("secret/gcp: path %q: missing version after %q", path, ":")
	}
	return before, after, nil
}

type accessSecretResponse struct {
	Name    string `json:"name"`
	Payload struct {
		Data string `json:"data"`
	} `json:"payload"`
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

func (r *Resolver) accessSecret(ctx context.Context, secretName, version string) ([]byte, error) {
	if strings.TrimSpace(r.cfg.ProjectID) == "" {
		return nil, fmt.Errorf("secret/gcp: project is not configured")
	}

	token, err := r.token.accessToken(ctx)
	if err != nil {
		return nil, err
	}

	apiURL, err := r.secretAccessURL(secretName, version)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("secret/gcp: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := r.cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultClientTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("secret/gcp: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("secret/gcp: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, apiErrorFromBody(resp.StatusCode, body)
	}

	var out accessSecretResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("secret/gcp: decode response: %w", err)
	}
	if out.Payload.Data == "" {
		return nil, fmt.Errorf("secret/gcp: response for %q has empty payload", secretName)
	}
	dec, err := base64.StdEncoding.DecodeString(out.Payload.Data)
	if err != nil {
		return nil, fmt.Errorf("secret/gcp: decode payload: %w", err)
	}
	return dec, nil
}

func (r *Resolver) secretAccessURL(secretName, version string) (string, error) {
	base := defaultSecretManagerHost
	if r.cfg.SecretManagerEndpoint != "" {
		base = strings.TrimRight(r.cfg.SecretManagerEndpoint, "/")
	}
	return fmt.Sprintf("%s/v1/projects/%s/secrets/%s/versions/%s:access",
		base,
		url.PathEscape(r.cfg.ProjectID),
		url.PathEscape(secretName),
		url.PathEscape(version),
	), nil
}

func apiErrorFromBody(status int, body []byte) error {
	var out accessSecretResponse
	if json.Unmarshal(body, &out) == nil && out.Error.Message != "" {
		return fmt.Errorf("secret/gcp: %s (HTTP %d)", out.Error.Message, status)
	}
	var generic struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &generic) == nil && generic.Error.Message != "" {
		return fmt.Errorf("secret/gcp: %s (HTTP %d)", generic.Error.Message, status)
	}
	return fmt.Errorf("secret/gcp: secret manager returned HTTP %d", status)
}
