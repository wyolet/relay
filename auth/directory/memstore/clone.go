package memstore

import (
	"encoding/json"
	"maps"
	"slices"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/rbac"
)

// The clones copy every slice and map, so neither a caller nor the store sees the other's later edits. Empty collections come back nil and a user's roles come back non-nil, as the Postgres store reads them.

func cloneUser(u *directory.User) *directory.User {
	if u == nil {
		return nil
	}
	c := *u
	c.Roles = append([]string{}, u.Roles...)
	return &c
}

func cloneMeta(m directory.Metadata) directory.Metadata {
	m.Labels = cloneStrings(m.Labels)
	m.Annotations = cloneStrings(m.Annotations)
	return m
}

func cloneStrings(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return maps.Clone(m)
}

func cloneAttributes(m map[string]json.RawMessage) map[string]json.RawMessage {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		out[k] = slices.Clone(v)
	}
	return out
}

func cloneBool(b *bool) *bool {
	if b == nil {
		return nil
	}
	v := *b
	return &v
}

func nilIfEmpty[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return slices.Clone(s)
}

func cloneGroup(g *directory.Group) *directory.Group {
	c := *g
	c.Meta = cloneMeta(g.Meta)
	c.Spec.MemberIDs = nilIfEmpty(g.Spec.MemberIDs)
	c.Spec.Enabled = cloneBool(g.Spec.Enabled)
	c.Spec.Attributes = cloneAttributes(g.Spec.Attributes)
	return &c
}

func cloneTeam(t *directory.Team) *directory.Team {
	c := *t
	c.Meta = cloneMeta(t.Meta)
	c.Spec.Enabled = cloneBool(t.Spec.Enabled)
	c.Spec.Attributes = cloneAttributes(t.Spec.Attributes)
	return &c
}

func cloneProject(p *directory.Project) *directory.Project {
	c := *p
	c.Meta = cloneMeta(p.Meta)
	c.Spec.Enabled = cloneBool(p.Spec.Enabled)
	c.Spec.Attributes = cloneAttributes(p.Spec.Attributes)
	return &c
}

func cloneServiceAccount(sa *directory.ServiceAccount) *directory.ServiceAccount {
	c := *sa
	c.Meta = cloneMeta(sa.Meta)
	c.Spec.Enabled = cloneBool(sa.Spec.Enabled)
	c.Spec.Attributes = cloneAttributes(sa.Spec.Attributes)
	return &c
}

func cloneRole(r *directory.Role) *directory.Role {
	c := *r
	c.Meta = cloneMeta(r.Meta)
	c.Spec.Rules = nil
	for _, rule := range r.Spec.Rules {
		c.Spec.Rules = append(c.Spec.Rules, rbac.Rule{Kinds: slices.Clone(rule.Kinds), Verbs: slices.Clone(rule.Verbs)})
	}
	c.Spec.Enabled = cloneBool(r.Spec.Enabled)
	c.Spec.Attributes = cloneAttributes(r.Spec.Attributes)
	return &c
}

func cloneBinding(b *directory.RoleBinding) *directory.RoleBinding {
	c := *b
	c.Meta = cloneMeta(b.Meta)
	c.Spec.Subjects = nilIfEmpty(b.Spec.Subjects)
	c.Spec.Enabled = cloneBool(b.Spec.Enabled)
	c.Spec.Attributes = cloneAttributes(b.Spec.Attributes)
	return &c
}
