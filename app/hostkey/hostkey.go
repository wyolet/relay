// Package hostkey is the domain layer for the HostKey entity — a credential
// the relay uses to authenticate to a Host (a serving endpoint). A HostKey
// is owned by the actor that created it (Meta.Owner.Kind=user, or
// =system for YAML-seeded keys) and *targets* a Host via Spec.HostID.
//
// Value modes:
//
//   - ValueKindEnv:    Spec.ValueFrom.Env names an environment variable the
//     relay reads at boot. No cleartext touches storage.
//   - ValueKindStored: cleartext is supplied on the write boundary
//     (Spec.Value), encrypted by the storage layer with
//     RELAY_MASTER_KEY, and persisted as opaque bytes.
//   - ValueKindOAuth:  Spec.Value carries an OAuth token blob (access +
//     refresh + expiry). Stored encrypted exactly like ValueKindStored; the
//     resolver returns the access token and refreshes it on expiry via the
//     oauth:<Provider> settings section. Spec.ValueFrom.Provider selects it.
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

	// Derived: populated by the control-API enrich step from the catalog
	// snapshot; not stored and not loaded from YAML. Lists every user
	// Policy whose Spec.HostKeyIDs references this HostKey, so the admin
	// UI can show where a key is in use.
	Policies []PolicyRef `json:"policies,omitempty" yaml:"-"`
}

// AnonIDPrefix prefixes the synthetic anonymous key's id, KeyHash, and name so
// it never collides with a real key (real KeyHash is 12 hex chars).
const AnonIDPrefix = "anon:"

// Pricing strategies — see Spec.PricingStrategy.
const (
	StrategyAPI = "api" // per-token metered spend
	StrategySub = "sub" // flat-rate subscription; reference cost only
)

// EffectivePricingStrategy returns the credential's strategy, defaulting an
// empty value to StrategyAPI.
func (s Spec) EffectivePricingStrategy() string {
	if s.PricingStrategy == "" {
		return StrategyAPI
	}
	return s.PricingStrategy
}

// Anonymous returns the synthetic, never-persisted HostKey routing injects for
// a host marked Spec.NoAuth — a keyless upstream (e.g. self-hosted Ollama).
// Resolved is empty so the adapter attaches no Authorization header. KeyHash is
// host-scoped (not derived from the empty value) so every no-auth host gets its
// own circuit breaker rather than sharing one. Spec.PolicyID is left empty: it
// is injected past the tier gate and carries no rate-limit tier.
func Anonymous(hostID, hostName string) *HostKey {
	return &HostKey{
		Meta:     meta.Metadata{ID: AnonIDPrefix + hostID, Name: AnonIDPrefix + hostName},
		Spec:     Spec{HostID: hostID},
		Resolved: "",
		KeyHash:  AnonIDPrefix + hostID,
	}
}

// PolicyRef is the lightweight {id, name} pair used in derived reverse-ref
// lists exposed by the control API.
type PolicyRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Spec carries non-secret config and the value-mode discriminator. Cleartext
// (Value) is write-only via YAML; storage encrypts and discards it.
type Spec struct {
	// HostID is the id of the Host this key authenticates to. Required.
	// Replaces the pre-refactor Meta.Owner pointing at a Host.
	HostID string `json:"hostId" yaml:"hostId" validate:"required"`

	// PolicyID is the id of the host-owned tier Policy this key mirrors.
	// Required: every key must declare an upstream tier so the pipeline
	// has rate-limit rules to apply. The referenced Policy must be host-
	// owned (Owner.Kind=host) AND its Owner.ID must equal HostID — i.e.
	// a key can only mirror a policy belonging to its own host. Cross-
	// entity validation lives in the composition layer.
	PolicyID    string    `json:"policyId" yaml:"policyId" validate:"required"`
	ValueFrom   ValueFrom `json:"valueFrom"             yaml:"valueFrom"             validate:"required"`
	DefaultTier string    `json:"defaultTier,omitempty" yaml:"defaultTier,omitempty" validate:"omitempty,slug"`
	Enabled     *bool     `json:"enabled,omitempty"     yaml:"enabled,omitempty"` // nil = true

	// PricingStrategy selects how this credential's spend is accounted.
	// "api": per-token metered — the cost is real spend and counts toward
	// future budget enforcement. "sub": flat-rate subscription — the cost
	// computed from the binding's pricing is a reference/"would-have-cost"
	// figure only, never enforced. Empty defaults to "api". The value must
	// be one of the credential's Host.Spec.PricingStrategies (the host's
	// menu of offered billing modes); that membership is a composition-layer
	// invariant, like Spec.PolicyID belonging to the host.
	PricingStrategy string `json:"pricingStrategy,omitempty" yaml:"pricingStrategy,omitempty" validate:"omitempty,oneof=api sub"`

	// Value is cleartext on the write path for ValueKindStored. Accepted
	// from JSON and YAML inputs; never returned on reads — Store.Get
	// never repopulates it, and marshalSpec strips it before any JSONB
	// persistence. The `omitempty` tag combined with that read-side
	// invariant keeps cleartext out of GET responses.
	Value string `json:"value,omitempty" yaml:"value,omitempty"`
}

// ValueFrom is the value-mode discriminator. For ValueKindEnv, Env names the
// environment variable. For ValueKindStored, Env is empty. For ValueKindOAuth,
// Provider names the oauth:<provider> settings section used to refresh the
// token, and Spec.Value (on write) carries the initial token blob.
type ValueFrom struct {
	Kind ValueKind `json:"kind"          yaml:"kind"          validate:"required,oneof=env stored oauth"`
	Env  string    `json:"env,omitempty" yaml:"env,omitempty"`
	// Provider selects the oauth:<provider> settings section (refresh
	// endpoints + client). Required for ValueKindOAuth, empty otherwise.
	Provider string `json:"provider,omitempty" yaml:"provider,omitempty"`
}

// ValueKind enumerates the supported value-storage modes.
type ValueKind string

const (
	ValueKindEnv    ValueKind = "env"
	ValueKindStored ValueKind = "stored"
	// ValueKindOAuth stores an OAuth token blob (access + refresh + expiry),
	// encrypted like ValueKindStored. The resolver returns the access token and
	// refreshes it on expiry via the oauth:<Provider> config. Same at-rest
	// storage as stored (rotated + decrypted by the same machinery); the
	// distinction is semantic and lives in this Spec, not the value_kind column.
	ValueKindOAuth ValueKind = "oauth"
)

// IsEnabled returns true when Enabled is unset or explicitly true.
func (k *HostKey) IsEnabled() bool { return k.Spec.Enabled == nil || *k.Spec.Enabled }

// Validate runs intra-row rules via the shared meta.Validator and enforces:
//   - Owner.Kind must be user or system (the actor that created the key);
//     keys are not owned by the Host they authenticate to.
//   - Spec.HostID is required; it points at the Host this key targets.
//   - Spec.PolicyID is required; it picks the upstream tier Policy this
//     key mirrors.
//   - ValueKindEnv requires Spec.ValueFrom.Env; cleartext Value must be empty.
//   - ValueKindStored requires non-empty cleartext Spec.Value at write time;
//     ValueFrom.Env must be empty.
//
// Cross-entity checks (HostID resolves to a Host; PolicyID resolves to a
// host-owned Policy of THAT host) live in the composition layer.
func (k *HostKey) Validate() error {
	if err := meta.Validator.Struct(k); err != nil {
		return err
	}
	switch k.Meta.Owner.Kind {
	case meta.OwnerUser, meta.OwnerSystem:
	default:
		return fmt.Errorf("hostkey %q: owner.kind must be user or system, got %q", k.Meta.Name, k.Meta.Owner.Kind)
	}
	if k.Spec.HostID == "" {
		return fmt.Errorf("hostkey %q: spec.hostId is required", k.Meta.Name)
	}
	if k.Spec.PolicyID == "" {
		return fmt.Errorf("hostkey %q: spec.policyId is required", k.Meta.Name)
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
	case ValueKindOAuth:
		if k.Spec.Value == "" && k.Resolved == "" {
			return fmt.Errorf("hostkey %q: value (oauth token blob) required for oauth mode", k.Meta.Name)
		}
		if k.Spec.ValueFrom.Env != "" {
			return fmt.Errorf("hostkey %q: valueFrom.env must be empty for oauth mode", k.Meta.Name)
		}
		if k.Spec.ValueFrom.Provider == "" {
			return fmt.Errorf("hostkey %q: valueFrom.provider required for oauth mode", k.Meta.Name)
		}
	}
	return nil
}
