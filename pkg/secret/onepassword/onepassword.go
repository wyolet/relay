//go:build cgo

// Package onepassword resolves secret.KindOnePassword refs via the official
// 1Password Go SDK and a service-account token. Fetch-only: plaintext is held
// in memory, never written to Postgres.
package onepassword

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	onepassword "github.com/1password/onepassword-sdk-go"

	"github.com/wyolet/relay/pkg/secret"
)

const opPrefix = "op://"

// Config holds service-account credentials for fetch-only resolution.
type Config struct {
	ServiceAccountToken string
	IntegrationName     string
	IntegrationVersion  string
}

// ConfigFromEnv reads OP_SERVICE_ACCOUNT_TOKEN. The second return value is false
// when the token is unset or empty.
func ConfigFromEnv() (Config, bool) {
	token := strings.TrimSpace(os.Getenv("OP_SERVICE_ACCOUNT_TOKEN"))
	if token == "" {
		return Config{}, false
	}
	return Config{ServiceAccountToken: token}, true
}

type secretsAPI interface {
	Resolve(ctx context.Context, secretReference string) (string, error)
}

// Resolver fetches secrets from 1Password (KindOnePassword only).
type Resolver struct {
	cfg Config

	newSecrets func(ctx context.Context) (secretsAPI, error)

	once    sync.Once
	client  secretsAPI
	initErr error
}

var _ secret.Resolver = (*Resolver)(nil)

// New returns a Resolver for secret.KindOnePassword refs.
func New(cfg Config) *Resolver {
	if cfg.IntegrationName == "" {
		cfg.IntegrationName = "wyolet-relay"
	}
	if cfg.IntegrationVersion == "" {
		cfg.IntegrationVersion = "0.0.0"
	}
	r := &Resolver{cfg: cfg}
	r.newSecrets = r.defaultNewSecrets
	return r
}

func (r *Resolver) defaultNewSecrets(ctx context.Context) (secretsAPI, error) {
	if strings.TrimSpace(r.cfg.ServiceAccountToken) == "" {
		return nil, fmt.Errorf("secret/onepassword: service account token is not configured")
	}
	client, err := onepassword.NewClient(ctx,
		onepassword.WithServiceAccountToken(r.cfg.ServiceAccountToken),
		onepassword.WithIntegrationInfo(r.cfg.IntegrationName, r.cfg.IntegrationVersion),
	)
	if err != nil {
		return nil, mapClientError(err)
	}
	return client.Secrets(), nil
}

func (r *Resolver) ensureClient(ctx context.Context) error {
	r.once.Do(func() {
		r.client, r.initErr = r.newSecrets(ctx)
	})
	return r.initErr
}

// Resolve returns the plaintext value for ref.Path = "op://<vault>/<item>/<field>".
func (r *Resolver) Resolve(ctx context.Context, ref secret.Ref) ([]byte, error) {
	if ref.Kind != secret.KindOnePassword {
		return nil, fmt.Errorf("secret/onepassword: wrong kind %q", ref.Kind)
	}
	if err := validatePath(ref.Path); err != nil {
		return nil, err
	}
	if err := r.ensureClient(ctx); err != nil {
		return nil, err
	}

	val, err := r.client.Resolve(ctx, ref.Path)
	if err != nil {
		return nil, mapResolveError(ref.Path, err)
	}
	if val == "" {
		return nil, fmt.Errorf("secret/onepassword: secret at %q is empty", ref.Path)
	}
	return []byte(val), nil
}

func validatePath(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("secret/onepassword: path is empty")
	}
	if !strings.HasPrefix(path, opPrefix) {
		return fmt.Errorf("secret/onepassword: path %q must start with %q", path, opPrefix)
	}
	if strings.TrimSpace(strings.TrimPrefix(path, opPrefix)) == "" {
		return fmt.Errorf("secret/onepassword: path %q is missing vault/item/field after %q", path, opPrefix)
	}
	return nil
}

func mapClientError(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "token"), strings.Contains(msg, "auth"), strings.Contains(msg, "unauthorized"):
		return fmt.Errorf("secret/onepassword: authentication failed: %w", err)
	default:
		return fmt.Errorf("secret/onepassword: create client: %w", err)
	}
}

func mapResolveError(path string, err error) error {
	if err == nil {
		return nil
	}

	var rateLimit *onepassword.RateLimitExceededError
	if errors.As(err, &rateLimit) {
		return fmt.Errorf("secret/onepassword: rate limit exceeded resolving %q", path)
	}

	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "not found"),
		strings.Contains(msg, "couldn't find"),
		strings.Contains(msg, "could not find"),
		strings.Contains(msg, "no item"):
		return fmt.Errorf("secret/onepassword: secret not found at %q", path)
	case strings.Contains(msg, "auth"),
		strings.Contains(msg, "token"),
		strings.Contains(msg, "unauthorized"),
		strings.Contains(msg, "forbidden"),
		strings.Contains(msg, "permission"):
		return fmt.Errorf("secret/onepassword: authentication failed resolving %q", path)
	case strings.Contains(msg, "invalid"),
		strings.Contains(msg, "malformed"),
		strings.Contains(msg, "reference"):
		return fmt.Errorf("secret/onepassword: invalid secret reference %q", path)
	default:
		return fmt.Errorf("secret/onepassword: resolve %q: %w", path, err)
	}
}
