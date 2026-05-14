// Package hostkey is the domain layer for the HostKey entity — a credential
// the relay uses to authenticate to a Host (a serving endpoint). A HostKey
// belongs to a Host (Meta.Owner.Kind=host, Meta.Owner.ID=host id).
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
package hostkey

import (
	"fmt"

	"github.com/wyolet/relay/app/meta"
)

// HostKey is a credential bound to a Host.
type HostKey struct {
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
	ValueFrom   ValueFrom `json:"valueFrom"             yaml:"valueFrom"             validate:"required"`
	DefaultTier string    `json:"defaultTier,omitempty" yaml:"defaultTier,omitempty" validate:"omitempty,slug"`
	Enabled     *bool     `json:"enabled,omitempty"     yaml:"enabled,omitempty"` // nil = true

	// Value is cleartext on the write path for ValueKindStored. Accepted
	// from JSON and YAML inputs; never returned on reads — Store.Get
	// never repopulates it, and marshalSpec strips it before any JSONB
	// persistence. The `omitempty` tag combined with that read-side
	// invariant keeps cleartext out of GET responses.
	Value string `json:"value,omitempty" yaml:"value,omitempty"`
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
func (k *HostKey) IsEnabled() bool { return k.Spec.Enabled == nil || *k.Spec.Enabled }

// Validate runs intra-row rules via the shared meta.Validator and enforces:
//   - Owner.Kind must be host; Owner.ID required.
//   - ValueKindEnv requires Spec.ValueFrom.Env; cleartext Value must be empty.
//   - ValueKindStored requires non-empty cleartext Spec.Value at write time;
//     ValueFrom.Env must be empty.
//
// Cross-entity checks (Owner.ID resolves to a Host) live in the composition
// layer.
func (k *HostKey) Validate() error {
	if err := meta.Validator.Struct(k); err != nil {
		return err
	}
	if k.Meta.Owner.Kind != meta.OwnerHost {
		return fmt.Errorf("hostkey %q: owner.kind must be host, got %q", k.Meta.Name, k.Meta.Owner.Kind)
	}
	if k.Meta.Owner.ID == "" {
		return fmt.Errorf("hostkey %q: owner.id is required (host id)", k.Meta.Name)
	}
	switch k.Spec.ValueFrom.Kind {
	case ValueKindEnv:
		if k.Spec.ValueFrom.Env == "" {
			return fmt.Errorf("hostkey %q: valueFrom.env required for env mode", k.Meta.Name)
		}
		if k.Spec.Value != "" {
			return fmt.Errorf("hostkey %q: value must be empty for env mode", k.Meta.Name)
		}
	case ValueKindStored:
		if k.Spec.Value == "" && k.Resolved == "" {
			return fmt.Errorf("hostkey %q: value required for stored mode", k.Meta.Name)
		}
		if k.Spec.ValueFrom.Env != "" {
			return fmt.Errorf("hostkey %q: valueFrom.env must be empty for stored mode", k.Meta.Name)
		}
	}
	return nil
}
