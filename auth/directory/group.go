package directory

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// ReservedGroupPrefix marks group names that stand for a built-in set of principals rather than a stored group, such as the all-authenticated subject. A stored group may not claim one.
const ReservedGroupPrefix = "system:"

// Group is a named set of users that a binding can name as a subject.
type Group struct {
	Meta Metadata
	Spec GroupSpec
}

// GroupSpec holds the members and the enabled flag.
type GroupSpec struct {
	// MemberIDs are user ids in the order they were added.
	MemberIDs []string
	// Enabled nil means enabled.
	Enabled *bool
	// Attributes are product fields stored next to the spec, keyed by field name; the module round-trips them untouched.
	Attributes map[string]json.RawMessage
}

// IsEnabled reports whether the group is enabled.
func (g *Group) IsEnabled() bool { return enabled(g.Spec.Enabled) }

func (g *Group) validate() error {
	// Ahead of the name rule, which would refuse these for the colon and hide why.
	if strings.HasPrefix(g.Meta.Name, ReservedGroupPrefix) {
		return fmt.Errorf("%w: group %q: names starting with %q are reserved", ErrInvalid, g.Meta.Name, ReservedGroupPrefix)
	}
	if err := validateMeta("group", &g.Meta, OwnerUser, OwnerSystem); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(g.Spec.MemberIDs))
	for _, id := range g.Spec.MemberIDs {
		if id == "" {
			return fmt.Errorf("%w: group %q: empty member id", ErrInvalid, g.Meta.Name)
		}
		if _, dup := seen[id]; dup {
			return fmt.Errorf("%w: group %q: duplicate member id %q", ErrInvalid, g.Meta.Name, id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

// validateMeta checks the name and, when owners is non-empty, that the owner kind is one of them.
func validateMeta(kind string, m *Metadata, owners ...string) error {
	if !ValidName(m.Name) {
		return fmt.Errorf("%w: %s %q: name must be a DNS-1123 label of at most %d characters", ErrInvalid, kind, m.Name, MaxNameLen)
	}
	if len(owners) > 0 && !slices.Contains(owners, m.Owner.Kind) {
		return fmt.Errorf("%w: %s %q: owner kind must be one of %v, got %q", ErrInvalid, kind, m.Name, owners, m.Owner.Kind)
	}
	return nil
}
