// Package group is the domain layer for the Group entity — a named set of
// users that role bindings can name as a subject. Local groups only: an
// IdP group of the same name unions with this one at login, and is never
// an object here.
//
// Authorization is app/authz's concern; this package validates shape only.
package group

import (
	"fmt"
	"strings"

	"github.com/wyolet/relay/app/meta"
)

// SystemPrefix marks the built-in virtual groups
// (system:authenticated, system:serviceaccounts, …). They are never rows,
// so a stored Group may not claim one of those names.
const SystemPrefix = "system:"

// Group is a named set of users.
type Group struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`
	// External is set on a group a provisioning source manages. The server owns it: a write that carries it is ignored, so no caller can hand a group to a source.
	External *External `json:"external,omitempty" yaml:"-" readOnly:"true" doc:"The provisioning source (a SCIM client) that manages the group, and its id there. Only that source, or an apply with force, changes the group."`
}

// External names the provisioning source that manages a group and the source's own id for it.
type External struct {
	Source string `json:"source"       doc:"Name of the SCIM client that provisioned the group."`
	ID     string `json:"id,omitempty" doc:"The group's id at that source."`
}

// Spec carries the member list and the enabled flag.
type Spec struct {
	// MemberIDs are User ids, in declaration order.
	MemberIDs []string `json:"memberIds,omitempty" yaml:"memberIds,omitempty" validate:"omitempty,dive,uuid" doc:"Ids of the member users."`

	// Enabled defaults to true when nil.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty" doc:"False stops role bindings on the group from granting anything. Omitted means true."`

	// PersonalAccount is the group's say on its members' personal accounts; empty leaves it to the deployment's accounts setting.
	PersonalAccount string `json:"personalAccount,omitempty" yaml:"-" validate:"omitempty,oneof=on off" enum:"on,off" doc:"Turns its members' personal accounts on or off. A member's own value wins over it; between groups, off wins. Omitted leaves it to the deployment's accounts setting; a disabled group says nothing. Set by an admin only, whoever owns the group (403 otherwise); a non-admin's write that leaves it out keeps the stored value."`
}

// PersonalAccountAttribute is the directory attribute the group's PersonalAccount is stored under.
const PersonalAccountAttribute = "personalAccount"

// IsEnabled returns true when Enabled is unset or explicitly true.
func (g *Group) IsEnabled() bool { return g.Spec.Enabled == nil || *g.Spec.Enabled }

// Validate runs intra-row rules via the shared meta.Validator and enforces:
//   - Owner.Kind is user or system.
//   - the name does not claim a built-in virtual group.
//   - no duplicate member ids.
func (g *Group) Validate() error {
	// Ahead of the slug rule, which would reject these names for the wrong
	// reason (the colon) and hide why they are unavailable.
	if strings.HasPrefix(g.Meta.Name, SystemPrefix) {
		return fmt.Errorf("group %q: names starting with %q are reserved for built-in groups", g.Meta.Name, SystemPrefix)
	}
	if err := meta.Validator.Struct(g); err != nil {
		return err
	}
	switch g.Meta.Owner.Kind {
	case meta.OwnerUser, meta.OwnerSystem:
	default:
		return fmt.Errorf("group %q: owner.kind must be user or system, got %q", g.Meta.Name, g.Meta.Owner.Kind)
	}
	seen := make(map[string]struct{}, len(g.Spec.MemberIDs))
	for _, id := range g.Spec.MemberIDs {
		if _, dup := seen[id]; dup {
			return fmt.Errorf("group %q: duplicate member id %q", g.Meta.Name, id)
		}
		seen[id] = struct{}{}
	}
	return nil
}
