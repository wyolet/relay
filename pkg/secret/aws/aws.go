// Package aws resolves secret.KindAWS refs by fetching from AWS Secrets
// Manager. SigV4 signing uses only the Go standard library (no AWS SDK).
// Fetch-only: plaintext is held in memory, never written to Postgres.
package aws

import (
	"bytes"
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
	serviceName   = "secretsmanager"
	targetGet     = "secretsmanager.GetSecretValue"
	contentType   = "application/x-amz-json-1.1"
	defaultClient = 30 * time.Second
)

// Credentials are static AWS API keys (no instance profile / SSO chain).
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// Config holds region and credentials for Secrets Manager fetch-only resolution.
type Config struct {
	Region      string
	Credentials Credentials
	// HTTPClient is optional; defaults to a 30s timeout client.
	HTTPClient *http.Client
	// Endpoint overrides the SM API host (tests only). Empty → regional endpoint.
	Endpoint string
	Now      func() time.Time
}

// ConfigFromEnv reads AWS_REGION, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY,
// and optional AWS_SESSION_TOKEN. Returns an error when required vars are missing.
func ConfigFromEnv() (Config, error) {
	region := strings.TrimSpace(os.Getenv("AWS_REGION"))
	ak := strings.TrimSpace(os.Getenv("AWS_ACCESS_KEY_ID"))
	sk := strings.TrimSpace(os.Getenv("AWS_SECRET_ACCESS_KEY"))
	if region == "" {
		return Config{}, fmt.Errorf("secret/aws: AWS_REGION is unset or empty")
	}
	if ak == "" || sk == "" {
		return Config{}, fmt.Errorf("secret/aws: AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY are required")
	}
	return Config{
		Region: region,
		Credentials: Credentials{
			AccessKeyID:     ak,
			SecretAccessKey: sk,
			SessionToken:    strings.TrimSpace(os.Getenv("AWS_SESSION_TOKEN")),
		},
	}, nil
}

// Resolver fetches secrets from AWS Secrets Manager (KindAWS only).
type Resolver struct {
	cfg Config
}

// New returns a Resolver for secret.KindAWS refs.
func New(cfg Config) *Resolver {
	return &Resolver{cfg: cfg}
}

func (r *Resolver) Resolve(ctx context.Context, ref secret.Ref) ([]byte, error) {
	if ref.Kind != secret.KindAWS {
		return nil, fmt.Errorf("secret/aws: wrong kind %q", ref.Kind)
	}
	secretName, jsonKey, err := parsePath(ref.Path)
	if err != nil {
		return nil, err
	}
	raw, err := r.getSecretValue(ctx, secretName)
	if err != nil {
		return nil, err
	}
	if jsonKey == "" {
		return raw, nil
	}
	return extractJSONKey(raw, jsonKey)
}

func parsePath(path string) (secretName, jsonKey string, err error) {
	if path == "" {
		return "", "", fmt.Errorf("secret/aws: path is empty")
	}
	before, after, ok := strings.Cut(path, ":")
	if !ok {
		return path, "", nil
	}
	if before == "" {
		return "", "", fmt.Errorf("secret/aws: path %q: missing secret name before %q", path, ":")
	}
	if after == "" {
		return "", "", fmt.Errorf("secret/aws: path %q: missing json key after %q", path, ":")
	}
	return before, after, nil
}

type getSecretRequest struct {
	SecretID string `json:"SecretId"`
}

type getSecretResponse struct {
	SecretString *string `json:"SecretString"`
	SecretBinary *string `json:"SecretBinary"`
}

type apiError struct {
	Type    string `json:"__type"`
	Message string `json:"Message"`
}

func (r *Resolver) getSecretValue(ctx context.Context, secretID string) ([]byte, error) {
	if strings.TrimSpace(r.cfg.Region) == "" {
		return nil, fmt.Errorf("secret/aws: region is not configured")
	}
	if r.cfg.Credentials.AccessKeyID == "" || r.cfg.Credentials.SecretAccessKey == "" {
		return nil, fmt.Errorf("secret/aws: credentials are not configured")
	}

	body, err := json.Marshal(getSecretRequest{SecretID: secretID})
	if err != nil {
		return nil, fmt.Errorf("secret/aws: marshal request: %w", err)
	}

	apiURL, signHost, err := r.apiEndpoint()
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("secret/aws: build request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Amz-Target", targetGet)

	now := time.Now
	if r.cfg.Now != nil {
		now = r.cfg.Now
	}
	req.Host = signHost
	if err := signRequest(req, body, r.cfg.Credentials, r.cfg.Region, serviceName, now()); err != nil {
		return nil, err
	}

	client := r.cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultClient}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("secret/aws: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("secret/aws: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, apiErrorFromBody(resp.StatusCode, respBody)
	}

	var out getSecretResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("secret/aws: decode response: %w", err)
	}
	if out.SecretString != nil {
		return []byte(*out.SecretString), nil
	}
	if out.SecretBinary != nil {
		dec, err := base64.StdEncoding.DecodeString(*out.SecretBinary)
		if err != nil {
			return nil, fmt.Errorf("secret/aws: decode SecretBinary: %w", err)
		}
		return dec, nil
	}
	return nil, fmt.Errorf("secret/aws: response for %q has neither SecretString nor SecretBinary", secretID)
}

func (r *Resolver) apiEndpoint() (requestURL, signHost string, err error) {
	if r.cfg.Endpoint != "" {
		if strings.Contains(r.cfg.Endpoint, "://") {
			u, err := url.Parse(r.cfg.Endpoint)
			if err != nil {
				return "", "", fmt.Errorf("secret/aws: parse endpoint: %w", err)
			}
			if u.Scheme == "" {
				u.Scheme = "https"
			}
			u.Path = "/"
			return u.String(), u.Host, nil
		}
		host := r.cfg.Endpoint
		return "https://" + host + "/", host, nil
	}
	host := serviceName + "." + r.cfg.Region + ".amazonaws.com"
	return "https://" + host + "/", host, nil
}

func apiErrorFromBody(status int, body []byte) error {
	var e apiError
	if json.Unmarshal(body, &e) == nil && (e.Type != "" || e.Message != "") {
		if e.Message != "" {
			return fmt.Errorf("secret/aws: %s (HTTP %d)", e.Message, status)
		}
		return fmt.Errorf("secret/aws: %s (HTTP %d)", e.Type, status)
	}
	return fmt.Errorf("secret/aws: secrets manager returned HTTP %d", status)
}

func extractJSONKey(raw []byte, key string) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("secret/aws: secret is not valid JSON for key %q: %w", key, err)
	}
	val, ok := doc[key]
	if !ok {
		return nil, fmt.Errorf("secret/aws: json key %q not found in secret", key)
	}
	if len(val) == 0 {
		return nil, fmt.Errorf("secret/aws: json key %q is empty", key)
	}
	// String values are quoted in JSON; unwrap when the value is a JSON string.
	var s string
	if err := json.Unmarshal(val, &s); err == nil {
		return []byte(s), nil
	}
	return val, nil
}
