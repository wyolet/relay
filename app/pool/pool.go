// Package pool is the domain layer for the Pool entity: an ordered set of one host's keys plus the selection algorithm that picks among them. Policies link pools; many policies may draw on one pool, and nothing in a pool is reserved for any of them.
//
// This package validates one row's shape, fills its defaults, and stores it. Out of scope: picking a key per request (app/keypool), checking that every key belongs to the pool's host (app/refcheck), and spend caps, which belong on the account.
package pool

import (
	"fmt"

	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/meta"
)

// DefaultSelection is the selection a pool that names none uses.
const DefaultSelection = keypool.KeySelectionPrioritized

// Pool is a set of host keys at one host.
type Pool struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`
}

// Spec names the host, its keys, and how a key is picked.
type Spec struct {
	// HostID is the one host every key in the pool authenticates to.
	HostID string `json:"hostId" yaml:"hostId" validate:"required,uuid"`

	// CredentialIDs are the pool's host keys. Order is the preference for prioritized selection. Empty is valid: deleting a key removes it from every pool, which may leave one empty.
	CredentialIDs []string `json:"credentialIds,omitempty" yaml:"credentialIds,omitempty" validate:"omitempty,dive,uuid"`

	Selection keypool.KeySelection `json:"selection,omitempty" yaml:"selection,omitempty" validate:"omitempty,oneof=prioritized round-robin least-recently-used"`
}

// Default fills the fields a wire form may omit, so a stored row and the manifest that declared it converge. Nil receiver is a no-op.
func (p *Pool) Default() {
	if p == nil {
		return
	}
	if p.Spec.Selection == "" {
		p.Spec.Selection = DefaultSelection
	}
}

// Validate runs intra-row rules via the shared meta.Validator and enforces:
//   - an owner of kind system, team, project or user; team and project owners carry an id.
//   - no key listed twice.
//
// Cross-row checks (the host exists; every key exists and authenticates to that host) live in app/refcheck.
func (p *Pool) Validate() error {
	if err := meta.Validator.Struct(p); err != nil {
		return err
	}
	switch p.Meta.Owner.Kind {
	case meta.OwnerSystem, meta.OwnerUser:
	case meta.OwnerTeam, meta.OwnerProject:
		if p.Meta.Owner.ID == "" {
			return fmt.Errorf("pool %q: owner.kind %q requires an id", p.Meta.Name, p.Meta.Owner.Kind)
		}
	default:
		return fmt.Errorf("pool %q: owner.kind must be system, team, project or user, got %q", p.Meta.Name, p.Meta.Owner.Kind)
	}
	seen := make(map[string]struct{}, len(p.Spec.CredentialIDs))
	for _, id := range p.Spec.CredentialIDs {
		if _, dup := seen[id]; dup {
			return fmt.Errorf("pool %q: duplicate credentialIds entry %q", p.Meta.Name, id)
		}
		seen[id] = struct{}{}
	}
	return nil
}
