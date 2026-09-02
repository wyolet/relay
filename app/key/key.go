// Package key is the domain layer for the Key entity — an inbound API key
// the relay issues to a client. Authentication only: plaintext is never
// stored, only sha256(KeyHash) plus a short display Prefix.
//
// A Key is a credential OF a principal (a ServiceAccount or a User), not
// an identity of its own, so rotating one leaves the principal — and
// everything bound to it — untouched.
package key

import (
	"fmt"
	"time"

	"github.com/wyolet/relay/app/meta"
)

// PrincipalKind names the sort of subject a Key authenticates as.
type PrincipalKind string

const (
	PrincipalServiceAccount PrincipalKind = "serviceaccount"
	PrincipalUser           PrincipalKind = "user"
)

// Principal is the subject a Key authenticates as. ID is a
// ServiceAccount id or a User id, per Kind.
type Principal struct {
	Kind PrincipalKind `json:"kind" yaml:"kind" validate:"required,oneof=serviceaccount user"`
	ID   string        `json:"id"   yaml:"id"   validate:"required,uuid"`
}

// Key is an inbound bearer credential.
type Key struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`
}

// Spec carries the auth material and gating flags. KeyHash is the sha256
// hex of the bearer token (lowercase, 64 chars); the plaintext is never
// stored anywhere.
type Spec struct {
	// Principal is the subject this key authenticates as. Required.
	Principal Principal `json:"principal" yaml:"principal" validate:"required"`

	// PolicyID is the Policy this Key serves under. **Optional** —
	// when empty, the key is "policy-less" and the relay's behavior is
	// controlled by the inference settings section
	// (AllowMissingPolicy). A policy-less key with that flag on is
	// allowed to reach any model served by any host the relay has
	// hostkeys for, with no policy-level rate limits. With the flag off,
	// such requests are rejected.
	PolicyID string `json:"policyId,omitempty" yaml:"policyId,omitempty" validate:"omitempty,uuid"`

	// KeyHash is sha256(plaintext) hex. Required and immutable after create.
	KeyHash string `json:"keyHash" yaml:"keyHash" validate:"required,len=64,hexadecimal"`

	// Prefix is the leading visible portion of the token (e.g. "rk_a8b3f2")
	// retained so the UI can show a recognisable identifier without ever
	// holding the plaintext.
	Prefix string `json:"prefix,omitempty" yaml:"prefix,omitempty"`

	// PreviousKeyHash is the hash rotation replaced. Server-set; accepted
	// alongside KeyHash until GraceUntil passes.
	PreviousKeyHash string `json:"previousKeyHash,omitempty" yaml:"-" validate:"omitempty,len=64,hexadecimal"`

	// GraceUntil bounds how long PreviousKeyHash still authenticates.
	// Server-set by rotate.
	GraceUntil *time.Time `json:"graceUntil,omitempty" yaml:"-"`

	// ExpiresAt, when non-nil, stops the key authenticating once passed.
	ExpiresAt *time.Time `json:"expiresAt,omitempty" yaml:"expiresAt,omitempty"`

	// RevokedAt, when non-nil, marks the key as rejected at auth time.
	RevokedAt *time.Time `json:"revokedAt,omitempty" yaml:"revokedAt,omitempty"`

	// Enabled defaults to true when nil.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`

	// PassthroughAllowed, when true, permits this key to forward an upstream
	// Authorization header verbatim to the provider. Gated by the relay's
	// global passthrough mode — when that mode is off the flag is moot.
	PassthroughAllowed bool `json:"passthroughAllowed,omitempty" yaml:"passthroughAllowed,omitempty"`

	// PayloadLoggingEnabled opts requests authenticated by this key into
	// full request/response body capture by the payloadlog observer. Off
	// by default. Independent of the policy-level flag — either enables
	// capture.
	PayloadLoggingEnabled bool `json:"payloadLoggingEnabled,omitempty" yaml:"payloadLoggingEnabled,omitempty"`
}

// IsEnabled returns true when Enabled is unset or explicitly true.
func (k *Key) IsEnabled() bool { return k.Spec.Enabled == nil || *k.Spec.Enabled }

// IsActive returns true when the key is enabled, not revoked, and not
// past its expiry at now.
func (k *Key) IsActive(now time.Time) bool {
	if !k.IsEnabled() || k.Spec.RevokedAt != nil {
		return false
	}
	return k.Spec.ExpiresAt == nil || now.Before(*k.Spec.ExpiresAt)
}

// InGrace reports whether the pre-rotation hash still authenticates at now.
func (k *Key) InGrace(now time.Time) bool {
	return k.Spec.PreviousKeyHash != "" && k.Spec.GraceUntil != nil && now.Before(*k.Spec.GraceUntil)
}

// Validate runs intra-row rules via the shared meta.Validator and enforces:
//   - Owner.Kind is project (serviceaccount principal) or user.
//   - a user principal is owned by that same user.
//
// The serviceaccount's own project is checked against the snapshot, not
// here: this package sees one row at a time.
func (k *Key) Validate() error {
	if err := meta.Validator.Struct(k); err != nil {
		return err
	}
	switch k.Spec.Principal.Kind {
	case PrincipalUser:
		if k.Meta.Owner.Kind != meta.OwnerUser {
			return fmt.Errorf("key %q: user-principal key must be owned by a user, got %q", k.Meta.Name, k.Meta.Owner.Kind)
		}
		if k.Meta.Owner.ID != "" && k.Meta.Owner.ID != k.Spec.Principal.ID {
			return fmt.Errorf("key %q: owner.id %q does not match principal.id %q", k.Meta.Name, k.Meta.Owner.ID, k.Spec.Principal.ID)
		}
	case PrincipalServiceAccount:
		if k.Meta.Owner.Kind != meta.OwnerProject {
			return fmt.Errorf("key %q: serviceaccount-principal key must be owned by a project, got %q", k.Meta.Name, k.Meta.Owner.Kind)
		}
	}
	return nil
}
