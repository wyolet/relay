// Package providerkey is the domain layer for the ProviderKey entity — a
// credential the relay uses to call an upstream Provider's API. A ProviderKey
// belongs to a Provider (Meta.Owner.Kind=provider, Meta.Owner.ID=provider id).
//
// Two value modes:
//
//   - ValueKindEnv:    Spec.ValueFrom.Env names an environment variable the
//                      relay reads at boot. No cleartext touches storage.
//   - ValueKindStored: cleartext is supplied on the write boundary
//                      (Spec.Value), encrypted by the storage layer with
//                      RELAY_MASTER_KEY, and persisted as opaque bytes.
//
// Spec.Value is intentionally tagged `json:"-"` so cleartext never appears in
// JSONB or in API responses. Loading from YAML is the only path that carries
// a literal value through this struct.
package providerkey

import (
	"fmt"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/ratelimit"
)

// ProviderKey is a credential bound to a Provider.
type ProviderKey struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`

	// Resolved is the cleartext value reconstructed at load time (read from
	// the env var or decrypted from PG). Runtime-only; never serialized.
	Resolved string `json:"-" yaml:"-"`
	// KeyHash is sha256 hex (first 6 bytes) of Resolved, for telemetry.
	KeyHash string `json:"-" yaml:"-"`
}

// Spec carries non-secret config and the value-mode discriminator. Cleartext
// (Value) is write-only via YAML; storage encrypts and discards it.
type Spec struct {
	ValueFrom   ValueFrom              `json:"valueFrom"             yaml:"valueFrom"             validate:"required"`
	DefaultTier string                 `json:"defaultTier,omitempty" yaml:"defaultTier,omitempty" validate:"omitempty,slug"`
	Enabled     *bool                  `json:"enabled,omitempty"     yaml:"enabled,omitempty"` // nil = true
	RateLimits  []ratelimit.Attachment `json:"rateLimits,omitempty"  yaml:"rateLimits,omitempty"  validate:"omitempty,dive"`

	// Value is cleartext on the write path for ValueKindStored. Never
	// serialised to JSON (so it never reaches JSONB or API responses); only
	// YAML readers populate it. Cleared by storage after encryption.
	Value string `json:"-" yaml:"value,omitempty"`
}

// ValueFrom is the value-mode discriminator. For ValueKindEnv, Env names the
// environment variable. For ValueKindStored, Env is empty.
type ValueFrom struct {
	Kind ValueKind `json:"kind"          yaml:"kind"          validate:"required,oneof=env stored"`
	Env  string    `json:"env,omitempty" yaml:"env,omitempty"`
}

// ValueKind enumerates the supported value-storage modes.
type ValueKind string

const (
	ValueKindEnv    ValueKind = "env"
	ValueKindStored ValueKind = "stored"
)

// IsEnabled returns true when Enabled is unset or explicitly true.
func (k *ProviderKey) IsEnabled() bool { return k.Spec.Enabled == nil || *k.Spec.Enabled }

// Validate runs intra-row rules via the shared meta.Validator and enforces:
//   - Owner.Kind must be provider; Owner.ID required.
//   - ValueKindEnv requires Spec.ValueFrom.Env; cleartext Value must be empty.
//   - ValueKindStored requires non-empty cleartext Spec.Value at write time;
//     ValueFrom.Env must be empty.
//
// Cross-entity checks (Owner.ID resolves to a Provider) live in the
// composition layer.
func (k *ProviderKey) Validate() error {
	if err := meta.Validator.Struct(k); err != nil {
		return err
	}
	if k.Meta.Owner.Kind != meta.OwnerProvider {
		return fmt.Errorf("providerkey %q: owner.kind must be provider, got %q", k.Meta.Name, k.Meta.Owner.Kind)
	}
	if k.Meta.Owner.ID == "" {
		return fmt.Errorf("providerkey %q: owner.id is required (provider id)", k.Meta.Name)
	}
	switch k.Spec.ValueFrom.Kind {
	case ValueKindEnv:
		if k.Spec.ValueFrom.Env == "" {
			return fmt.Errorf("providerkey %q: valueFrom.env required for env mode", k.Meta.Name)
		}
		if k.Spec.Value != "" {
			return fmt.Errorf("providerkey %q: value must be empty for env mode", k.Meta.Name)
		}
	case ValueKindStored:
		if k.Spec.Value == "" && k.Resolved == "" {
			return fmt.Errorf("providerkey %q: value required for stored mode", k.Meta.Name)
		}
		if k.Spec.ValueFrom.Env != "" {
			return fmt.Errorf("providerkey %q: valueFrom.env must be empty for stored mode", k.Meta.Name)
		}
	}
	return nil
}
