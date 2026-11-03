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
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wyolet/relay/app/meta"
)

// HostKey is a credential bound to a Host.
type HostKey struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`

	// Status is observed state (see Status type) — API-visible, never
	// round-tripped through YAML manifests.
	Status Status `json:"status,omitzero" yaml:"-" readOnly:"true" doc:"Observed state of the credential. Ignored on write."`
	Spec   Spec   `json:"spec"     yaml:"spec"`

	// Resolved is the cleartext value reconstructed at load time (read from
	// the env var or decrypted from PG). Runtime-only; never serialized.
	Resolved string `json:"-" yaml:"-"`
	// KeyHash is sha256 hex (first 6 bytes) of Resolved, for telemetry.
	KeyHash string `json:"-" yaml:"-"`

	// Derived: populated by the control-API enrich step from the catalog
	// snapshot; not stored and not loaded from YAML. Lists every Policy
	// linking this HostKey in Spec.CredentialIDs, so the admin UI can show
	// where a key is in use.
	Policies []PolicyRef `json:"policies,omitempty" yaml:"-" readOnly:"true" doc:"Policies that link this key, directly or through a pool. Derived on read; ignored on write."`
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
// own circuit breaker rather than sharing one. It has no account, so no tier
// caps it.
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
	ID   string `json:"id"   doc:"Policy id."`
	Name string `json:"name" doc:"Policy slug."`
}

// Spec carries non-secret config and the value-mode discriminator. Cleartext
// (Value) is write-only via YAML; storage encrypts and discards it.
type Spec struct {
	// HostID is the id of the Host this key authenticates to. Required.
	// Replaces the pre-refactor Meta.Owner pointing at a Host.
	HostID string `json:"hostId" yaml:"hostId" validate:"required" doc:"Id of the host this key authenticates to."`

	// AccountID is the provider account (organization) the key belongs to; keys of one account share its upstream limits, and the account's bound tier policy caps them. Required: nothing makes an account for a key. The account must be at HostID, a cross-row check in app/refcheck.
	AccountID string `json:"accountId,omitempty" yaml:"accountId,omitempty" validate:"omitempty,uuid" doc:"Id of the provider account (organization) the key belongs to. Keys of one account share its upstream limits, and the policy bound to the account caps them. The account must be at hostId and have the key's owner. Required on create and update: a key without one answers 400 account_required; create the provider account at the host first."`

	ValueFrom   ValueFrom `json:"valueFrom"             yaml:"valueFrom"             validate:"required" doc:"Where the secret comes from."`
	DefaultTier string    `json:"defaultTier,omitempty" yaml:"defaultTier,omitempty" validate:"omitempty,slug" deprecated:"true" doc:"Stored but not applied to requests. Upstream tier limits come from the policy bound to the key's account."`
	Enabled     *bool     `json:"enabled,omitempty"     yaml:"enabled,omitempty"     doc:"False stops routing requests to this key. Omitted means true."` // nil = true

	// PricingStrategy selects how this credential's spend is accounted.
	// "api": per-token metered — the cost is real spend and counts toward
	// future budget enforcement. "sub": flat-rate subscription — the cost
	// computed from the binding's pricing is a reference/"would-have-cost"
	// figure only, never enforced. Empty defaults to "api". The value must
	// be one of the credential's Host.Spec.PricingStrategies (the host's
	// menu of offered billing modes); that membership is a composition-layer
	// invariant.
	PricingStrategy string `json:"pricingStrategy,omitempty" yaml:"pricingStrategy,omitempty" validate:"omitempty,oneof=api sub" enum:"api,sub" doc:"How the key's spend is accounted: api is metered spend that counts toward budgets; sub is a flat-rate subscription whose computed cost is a reference figure only. Must be one of the host's pricingStrategies. Omitted means api."`

	// Value is cleartext on the write path for ValueKindStored. Accepted
	// from JSON and YAML inputs; never returned on reads — Store.Get
	// never repopulates it, and marshalSpec strips it before any JSONB
	// persistence. The `omitempty` tag combined with that read-side
	// invariant keeps cleartext out of GET responses.
	Value string `json:"value,omitempty" yaml:"value,omitempty" writeOnly:"true" doc:"The secret, for valueFrom.kind stored or oauth (an OAuth token blob). Sealed at rest and never returned. Set on create only: an update keeps the stored value and refuses a new one (use POST /host-keys/by-id/{id}/rotate)."`
}

// ValueFrom is the value-mode discriminator. For ValueKindEnv, Env names the
// environment variable. For ValueKindStored, Env is empty. For ValueKindOAuth,
// Provider names the oauth:<provider> settings section used to refresh the
// token, and Spec.Value (on write) carries the initial token blob.
type ValueFrom struct {
	Kind ValueKind `json:"kind"          yaml:"kind"          validate:"required,oneof=env stored oauth" enum:"env,stored,oauth" doc:"env reads the secret from an environment variable; stored seals the value sent in spec.value; oauth stores an OAuth token blob from spec.value and refreshes it."`
	Env  string    `json:"env,omitempty" yaml:"env,omitempty" doc:"Environment variable holding the secret, for kind env. RELAY_* and WYOLET_* names are refused."`
	// Provider selects the oauth:<provider> settings section (refresh
	// endpoints + client). Required for ValueKindOAuth, empty otherwise.
	Provider string `json:"provider,omitempty" yaml:"provider,omitempty" doc:"OAuth provider whose settings section refreshes the token, for kind oauth."`
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

// reservedEnvPrefixes name relay's own configuration (master key, admin token, DSNs, sign-in secrets). A key's value is sent to its host, so an env ref under them would hand that configuration to whoever controls the host.
var reservedEnvPrefixes = []string{"RELAY_", "WYOLET_"}

// ErrReservedEnv refuses an env ref under a reserved name. It leaves the variable name out: the reason is shown to anyone who can read the key.
var ErrReservedEnv = errors.New("valueFrom.env names a variable reserved for relay's own configuration (RELAY_*, WYOLET_*)")

// ErrNoAccount refuses a key that names no provider account.
var ErrNoAccount = errors.New("a host key needs a provider account; create one at this host first")

// reservedEnv reports whether name falls under relay's own configuration. Case is ignored because some platforms resolve env names case-insensitively.
func reservedEnv(name string) bool {
	upper := strings.ToUpper(name)
	for _, p := range reservedEnvPrefixes {
		if strings.HasPrefix(upper, p) {
			return true
		}
	}
	return false
}

// Shown prefix bounds: a short secret shows nothing, so a prefix is never a large part of one.
const (
	prefixLen          = 8
	minLenToShowPrefix = 24
)

// Prefix is the secret's first characters, for the key's owners to recognise it; "" while it is unresolved or too short to show any of it.
func (k *HostKey) Prefix() string {
	if len(k.Resolved) < minLenToShowPrefix {
		return ""
	}
	return k.Resolved[:prefixLen]
}

// formatPrefixes are the fixed starts of provider key formats, which carry no secret. Longer ones come first, so the most specific matches.
var formatPrefixes = []string{"sk-proj-", "sk-svcacct-", "sk-ant-", "sk-or-", "sk-", "AIza", "gsk_", "xai-", "pplx-", "hf_", "nvapi-", "r8_"}

// Hint is what anyone but the key's owners sees of the secret: the key format's fixed prefix, when it starts with a known one, and the last 4 characters; "" while it is unresolved or too short to show any of it.
func (k *HostKey) Hint() string {
	v := k.Resolved
	if len(v) < minLenToShowPrefix {
		return ""
	}
	format := ""
	for _, p := range formatPrefixes {
		if strings.HasPrefix(v, p) {
			format = p
			break
		}
	}
	return format + "..." + v[len(v)-4:]
}

// IsEnabled returns true when Enabled is unset or explicitly true.
func (k *HostKey) IsEnabled() bool { return k.Spec.Enabled == nil || *k.Spec.Enabled }

// Validate runs intra-row rules via the shared meta.Validator and enforces:
//   - Owner.Kind must be user, system, team or project, a team owner with its
//     id; keys are not owned by the Host they authenticate to.
//   - Spec.HostID is required; it points at the Host this key targets.
//   - ValueKindEnv requires Spec.ValueFrom.Env outside the reserved RELAY_*/WYOLET_* names; cleartext Value must be empty.
//   - ValueKindStored requires non-empty cleartext Spec.Value at write time;
//     ValueFrom.Env must be empty.
//
// Cross-entity checks (HostID resolves to a Host; AccountID names an account
// at THAT host) live in app/refcheck.
func (k *HostKey) Validate() error {
	if err := meta.Validator.Struct(k); err != nil {
		return err
	}
	switch k.Meta.Owner.Kind {
	case meta.OwnerUser, meta.OwnerSystem, meta.OwnerProject:
	case meta.OwnerTeam:
		if k.Meta.Owner.ID == "" {
			return fmt.Errorf("hostkey %q: a team owner needs the team's id", k.Meta.Name)
		}
	default:
		return fmt.Errorf("hostkey %q: owner.kind must be user, system, team or project, got %q", k.Meta.Name, k.Meta.Owner.Kind)
	}
	if k.Spec.HostID == "" {
		return fmt.Errorf("hostkey %q: spec.hostId is required", k.Meta.Name)
	}
	switch k.Spec.ValueFrom.Kind {
	case ValueKindEnv:
		if k.Spec.ValueFrom.Env == "" {
			return fmt.Errorf("hostkey %q: valueFrom.env required for env mode", k.Meta.Name)
		}
		if reservedEnv(k.Spec.ValueFrom.Env) {
			return fmt.Errorf("hostkey %q: %w", k.Meta.Name, ErrReservedEnv)
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

// Status is the entity's observed state — written by relay components
// (never by the operator, never seeded from YAML), returned on the same
// hostkey the UI already reads. Spec stays desired-state.
type Status struct {
	// Credential reports the OAuth credential's refresh lifecycle. Nil for
	// api-key hostkeys and for oauth keys with no observation yet.
	Credential *CredentialStatus `json:"credential,omitempty" yaml:"-" doc:"Refresh health of an oauth credential. Absent for other kinds and before the first renewal."`

	// Unresolved is set at load time when the key's secret could not be
	// read (an env-ref whose variable is unset, a stored value the master
	// key cannot decrypt). The row still lists and edits so an operator can
	// repair it; the snapshot drops it so no request picks a valueless key.
	// Load-time only — never written to the status column.
	Unresolved *UnresolvedStatus `json:"unresolved,omitempty" yaml:"-" doc:"Set when the secret could not be read at load; the key is not routed until repaired."`
}

// UnresolvedStatus explains why a key's value could not be resolved.
type UnresolvedStatus struct {
	Reason string `json:"reason" doc:"Why the secret could not be read, such as an unset environment variable."`
}

// CredentialState classifies an OAuth credential's refresh health.
type CredentialState string

const (
	// CredentialOK: last renewal succeeded; ExpiresAt is the new expiry.
	CredentialOK CredentialState = "ok"
	// CredentialRevoked: the provider rejected the grant itself — renewal
	// stops until the operator re-authorizes (uploading a new value clears
	// the status).
	CredentialRevoked CredentialState = "revoked"
)

// CredentialStatus is one observation of the credential's refresh health.
type CredentialStatus struct {
	State     CredentialState `json:"state"               yaml:"-" enum:"ok,revoked" doc:"ok after a successful renewal; revoked when the provider rejected the grant, until a new value is uploaded."`
	ExpiresAt time.Time       `json:"expiresAt,omitempty" yaml:"-" doc:"When the current access token expires."`
	RenewedAt time.Time       `json:"renewedAt,omitempty" yaml:"-" doc:"When the token was last renewed."`
	LastError string          `json:"lastError,omitempty" yaml:"-" doc:"The last renewal error, when it failed."`
	At        time.Time       `json:"at"                  yaml:"-" doc:"When this observation was recorded."`
}
