// Package account is the domain layer for the Account entity: one provider organization at one host. Upstream tier limits are counted per account, so several host keys of one organization share its capacity; adding a key adds failover and rotation, not throughput.
//
// This package validates one row's shape and stores it. Out of scope: learning org ids from upstream responses, creating an owner's default account on first use, and applying an account's tier policy to requests.
package account

import (
	"fmt"

	"github.com/wyolet/relay/app/meta"
)

// Account is one provider organization at one host.
type Account struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`
}

// Spec names the host and, once known, the provider's organization id.
type Spec struct {
	// HostID is the host this account is held at.
	HostID string `json:"hostId" yaml:"hostId" validate:"required,uuid"`

	// OrgID is the provider's organization id: the org, never a person or an email, since one login can belong to several orgs with separate limits. Empty until known.
	OrgID string `json:"orgId,omitempty" yaml:"orgId,omitempty"`
}

// Validate runs intra-row rules via the shared meta.Validator and requires an owner of kind system, team, project or user; team and project owners carry an id.
//
// Cross-row checks (the host exists) live in app/refcheck.
func (a *Account) Validate() error {
	if err := meta.Validator.Struct(a); err != nil {
		return err
	}
	switch a.Meta.Owner.Kind {
	case meta.OwnerSystem, meta.OwnerUser:
	case meta.OwnerTeam, meta.OwnerProject:
		if a.Meta.Owner.ID == "" {
			return fmt.Errorf("account %q: owner.kind %q requires an id", a.Meta.Name, a.Meta.Owner.Kind)
		}
	default:
		return fmt.Errorf("account %q: owner.kind must be system, team, project or user, got %q", a.Meta.Name, a.Meta.Owner.Kind)
	}
	return nil
}
