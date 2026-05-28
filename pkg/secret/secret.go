// Package secret is the relay's unified secret-resolution layer: a
// backend-agnostic Ref pointing at a credential, and a Resolver that turns
// it into plaintext. It is the foundation for pluggable secret backends —
// today the built-in env and stored (AES-GCM in Postgres) resolvers, later
// vendorable subpackages for Vault, AWS Secrets Manager, etc.
//
// Every secret the relay handles — upstream HostKey credentials AND the
// relay's own native secrets (e.g. object-store keys) — resolves through
// this one seam, so adding a backend is additive rather than a rewrite.
//
// pkg purity: this package imports only pkg/crypto. The stored resolver's
// persistence is injected via the Store interface, whose Postgres
// implementation lives in app/ — keeping pkg/secret vendorable.
package secret

import (
	"context"
	"fmt"
)

// Kind identifies which backend resolves a Ref.
type Kind string

const (
	// KindEnv reads plaintext from an OS environment variable. No
	// ciphertext is ever persisted.
	KindEnv Kind = "env"
	// KindStored decrypts AES-GCM ciphertext held in the relay's own
	// store (Postgres today) using the master key.
	KindStored Kind = "stored"
	// KindAWS fetches a secret from AWS Secrets Manager. Fetch-only: the
	// secret lives in AWS, relay holds the resolved value in memory only.
	// Locator is Path = "<secretName>[:<jsonKey>]".
	KindAWS Kind = "aws"
	// KindBitwarden fetches a secret from a Bitwarden/Vaultwarden server,
	// decrypted client-side. Fetch-only; held in memory only. Locator is
	// Path = "<itemNameOrID>[/<field>]" (field defaults to "password").
	KindBitwarden Kind = "bitwarden"
)

// pathAddressed lists the kinds whose locator is the generic Ref.Path —
// external fetch-only backends. Their resolvers live in pkg/secret/<kind>
// and are registered by the composition layer.
var pathAddressed = map[Kind]struct{}{
	KindAWS:       {},
	KindBitwarden: {},
}

// Ref is a backend-agnostic pointer to a secret. It is JSON-serializable
// so it can live inside a spec or settings JSONB blob. Exactly one
// locator field is meaningful per Kind.
type Ref struct {
	Kind Kind `json:"kind"`

	// Env is the OS variable name when Kind == KindEnv.
	Env string `json:"env,omitempty"`

	// ID is the backend key when Kind == KindStored (and future
	// id-addressed backends): the secret_values row id.
	ID string `json:"id,omitempty"`

	// Path is the locator for path-addressed external backends (KindAWS,
	// KindBitwarden, …): an opaque per-backend reference string the
	// resolver parses (e.g. "prod/openai-key:apiKey", "openai-key/password").
	Path string `json:"path,omitempty"`
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

// Resolver turns a Ref into plaintext. Implementations handle one Kind and
// register with a Registry. Resolution is expected off the hot path
// (load-time / boot-time), so implementations may do I/O.
type Resolver interface {
	Resolve(ctx context.Context, ref Ref) ([]byte, error)
}

// Writer is the optional create side: a backend that can persist a new
// secret and hand back a Ref addressing it. env-style backends are
// read-only and do not implement it.
type Writer interface {
	// Create persists plaintext under id and returns a Ref resolving to
	// it. Overwrites any existing secret at id.
	Create(ctx context.Context, id string, plaintext []byte) (Ref, error)
}
