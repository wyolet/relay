// Package secret resolves secret references: a backend-agnostic, JSON-serializable Ref names where a credential lives, and a Resolver registered for the Ref's Kind turns it into plaintext. A Registry dispatches by Kind; EnvResolver reads the process environment.
//
// Refs are meant to be persisted, so the JSON shape of Ref is stable. Backends that hold ciphertext or talk to an external secret store are implemented by the consumer or in sibling packages and registered at startup; this package does no I/O beyond the environment and has no dependencies outside the standard library.
package secret

import (
	"context"
	"fmt"
	"time"
)

// Kind identifies which backend resolves a Ref.
type Kind string

const (
	// KindEnv reads plaintext from an OS environment variable. No ciphertext is ever persisted.
	KindEnv Kind = "env"
	// KindStored decrypts ciphertext the consumer holds in its own store, addressed by Ref.ID.
	KindStored Kind = "stored"
	// KindOAuth resolves an OAuth credential: the stored value (Ref.ID) is a token blob (access + refresh + expiry) and the resolver returns the access token, refreshing it when expired. Ref.Provider names the provider configuration used to refresh.
	KindOAuth Kind = "oauth"
	// KindAWS fetches a secret from AWS Secrets Manager, held in memory only. Locator is Path = "<secretName>[:<jsonKey>]".
	KindAWS Kind = "aws"
	// KindBitwarden fetches a secret from a Bitwarden/Vaultwarden server, decrypted client-side, held in memory only. Locator is Path = "<itemNameOrID>[/<field>]" (field defaults to "password").
	KindBitwarden Kind = "bitwarden"
	// KindGCP fetches a secret version from GCP Secret Manager, held in memory only. Locator is Path = "<secretName>[:<version>]" (version defaults to "latest"); the project comes from resolver config.
	KindGCP Kind = "gcp"
	// KindAzure fetches a secret from an Azure Key Vault, held in memory only. Locator is Path = "<secretName>[/<version>]"; the vault URL comes from resolver config.
	KindAzure Kind = "azure"
	// KindOnePassword fetches a secret from 1Password with a service-account token, held in memory only. Locator is Path = "op://<vault>/<item>/<field>".
	KindOnePassword Kind = "onepassword"
)

// pathAddressed lists the kinds whose locator is the generic Ref.Path: external fetch-only backends, whose resolvers the consumer registers.
var pathAddressed = map[Kind]struct{}{
	KindAWS:         {},
	KindBitwarden:   {},
	KindGCP:         {},
	KindAzure:       {},
	KindOnePassword: {},
}

// Ref is a backend-agnostic pointer to a secret. Its JSON form is persisted by consumers, so field names and tags are stable. Exactly one locator field is meaningful per Kind.
type Ref struct {
	Kind Kind `json:"kind"`

	// Env is the OS variable name when Kind == KindEnv.
	Env string `json:"env,omitempty"`

	// ID is the backend's key for the secret when Kind is KindStored or KindOAuth.
	ID string `json:"id,omitempty"`

	// Path is the locator for path-addressed external backends (KindAWS, KindBitwarden, …): an opaque per-backend string the resolver parses (e.g. "prod/api-key:value", "api-key/password").
	Path string `json:"path,omitempty"`

	// Provider names the OAuth provider configuration that refreshes this credential. Meaningful only when Kind == KindOAuth.
	Provider string `json:"provider,omitempty"`
}

// Validate checks the Ref has the locator its Kind requires.
func (r Ref) Validate() error {
	switch r.Kind {
	case KindEnv:
		if r.Env == "" {
			return fmt.Errorf("secret: env ref requires a non-empty env var name")
		}
	case KindStored:
		if r.ID == "" {
			return fmt.Errorf("secret: stored ref requires a non-empty id")
		}
	case KindOAuth:
		if r.ID == "" {
			return fmt.Errorf("secret: oauth ref requires a non-empty id")
		}
		if r.Provider == "" {
			return fmt.Errorf("secret: oauth ref requires a non-empty provider")
		}
	default:
		if _, ok := pathAddressed[r.Kind]; ok {
			if r.Path == "" {
				return fmt.Errorf("secret: %s ref requires a non-empty path", r.Kind)
			}
			return nil
		}
		return fmt.Errorf("secret: unknown kind %q", r.Kind)
	}
	return nil
}

// Resolver turns a Ref into plaintext. Implementations handle one Kind and register with a Registry. Resolution is expected at load time, not per request, so implementations may do I/O.
type Resolver interface {
	Resolve(ctx context.Context, ref Ref) ([]byte, error)
}

// Writer is the optional create side: a backend that can persist a new secret and hand back a Ref addressing it. Read-only backends such as env do not implement it.
type Writer interface {
	// Create persists plaintext under id and returns a Ref resolving to it. Overwrites any existing secret at id.
	Create(ctx context.Context, id string, plaintext []byte) (Ref, error)
}

// Credential is a resolved secret that may expire, such as a short-lived token from a workload identity.
type Credential struct {
	Value []byte
	// ExpiresAt is when Value stops being valid; the zero time means it does not expire.
	ExpiresAt time.Time
}

// CredentialResolver is the optional side of a Resolver whose values expire, so the caller can refresh them before ExpiresAt.
type CredentialResolver interface {
	ResolveCredential(ctx context.Context, ref Ref) (Credential, error)
}
