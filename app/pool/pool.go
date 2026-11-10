// Package pool is the domain layer for the Pool entity: an ordered set of one host's keys, listed one by one or as whole provider accounts, plus the selection algorithm that picks among them. Policies link pools; many policies may draw on one pool, and nothing in a pool is reserved for any of them.
//
// This package validates one row's shape, fills its defaults, and stores it. Out of scope: picking a key per request (app/keypool), checking that every key belongs to the pool's host (app/refcheck), and spend caps, which belong on the account.
package pool

import (
	"fmt"

	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/pkg/meta"
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
	HostID string `json:"hostId" yaml:"hostId" validate:"required,uuid" doc:"Id of the one host every key in the pool authenticates to."`

	// AccountIDs are provider accounts the pool draws on whole: each contributes every key it holds now or later, a shared account the keys of its real account. They come before CredentialIDs in prioritized order.
	AccountIDs []string `json:"accountIds,omitempty" yaml:"accountIds,omitempty" validate:"omitempty,dive,uuid" doc:"Ids of provider accounts at hostId the pool draws on whole: each contributes every host key it holds, now and later, and a shared account the keys of its real account. Prioritized selection tries the accounts first, in this order and each account's keys in creation order, then credentialIds in their order; a key listed directly and through its account counts once, at its first place. May be empty."`

	// CredentialIDs are the pool's host keys. Order is the preference for prioritized selection. Empty is valid: deleting a key removes it from every pool, which may leave one empty.
	CredentialIDs []string `json:"credentialIds,omitempty" yaml:"credentialIds,omitempty" validate:"omitempty,dive,uuid" doc:"Ids of the pool's host keys, all at hostId, tried after the keys of accountIds. The order is the preference for prioritized selection. May be empty."`

	Selection keypool.KeySelection `json:"selection,omitempty" yaml:"selection,omitempty" validate:"omitempty,oneof=prioritized round-robin least-recently-used" enum:"prioritized,round-robin,least-recently-used" doc:"How a request picks a key: prioritized takes the first healthy key in order, round-robin rotates, least-recently-used takes the key idle longest. Omitted means prioritized."`
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
//   - no key and no account listed twice.
//
// Cross-row checks (the host exists; every key and account exists and is at that host) live in app/refcheck.
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
	if err := noDuplicates(p.Meta.Name, "accountIds", p.Spec.AccountIDs); err != nil {
		return err
	}
	return noDuplicates(p.Meta.Name, "credentialIds", p.Spec.CredentialIDs)
}

func noDuplicates(pool, field string, ids []string) error {
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			return fmt.Errorf("pool %q: duplicate %s entry %q", pool, field, id)
		}
		seen[id] = struct{}{}
	}
	return nil
}
