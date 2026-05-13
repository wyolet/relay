// Package relaykey is the domain layer for the RelayKey entity — an
// inbound API key the relay issues to a client. Authentication only:
// plaintext is never stored, only sha256(KeyHash) plus a short display
// Prefix.
//
// RelayKey does not reference Policy. Policy selection is driven entirely
// by the incoming request's model — model resolves to Provider, Provider
// names its DefaultPolicy. The per-key policy override that existed in the
// old design is gone.
package relaykey

import (
	"fmt"
	"time"

	"github.com/wyolet/relay/app/meta"
)

// RelayKey is an inbound bearer credential.
type RelayKey struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`
}

// Spec carries the auth material and gating flags. KeyHash is the sha256
// hex of the bearer token (lowercase, 64 chars); the plaintext is never
// stored anywhere.
type Spec struct {
	// KeyHash is sha256(plaintext) hex. Required and immutable after create.
	KeyHash string `json:"keyHash" yaml:"keyHash" validate:"required,len=64,hexadecimal"`

	// Prefix is the leading visible portion of the token (e.g. "rk_a8b3f2")
	// retained so the UI can show a recognisable identifier without ever
	// holding the plaintext.
	Prefix string `json:"prefix,omitempty" yaml:"prefix,omitempty"`

	// RevokedAt, when non-nil, marks the key as rejected at auth time.
	RevokedAt *time.Time `json:"revokedAt,omitempty" yaml:"revokedAt,omitempty"`

	// Enabled defaults to true when nil.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`

	// PassthroughAllowed, when true, permits this key to forward an upstream
	// Authorization header verbatim to the provider. Gated by the relay's
	// global passthrough mode — when that mode is off the flag is moot.
	PassthroughAllowed bool `json:"passthroughAllowed,omitempty" yaml:"passthroughAllowed,omitempty"`
}

// IsEnabled returns true when Enabled is unset or explicitly true.
func (k *RelayKey) IsEnabled() bool { return k.Spec.Enabled == nil || *k.Spec.Enabled }

// IsActive returns true when the key is enabled and not revoked.
func (k *RelayKey) IsActive() bool { return k.IsEnabled() && k.Spec.RevokedAt == nil }

// Validate runs intra-row rules via the shared meta.Validator and enforces:
//   - Owner.Kind is user (RelayKeys are operator-issued).
func (k *RelayKey) Validate() error {
	if err := meta.Validator.Struct(k); err != nil {
		return err
	}
	if k.Meta.Owner.Kind != meta.OwnerUser {
		return fmt.Errorf("relaykey %q: owner.kind must be user, got %q", k.Meta.Name, k.Meta.Owner.Kind)
	}
	return nil
}
