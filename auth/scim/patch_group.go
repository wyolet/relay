package scim

import (
	"encoding/json"
	"slices"

	"github.com/wyolet/relay/auth/directory"
)

// groupPatch is a group as PATCH changes it. index mirrors members, so a push of many members stays linear.
type groupPatch struct {
	displayName, externalID string
	members                 []string
	index                   map[string]bool
}

func newGroupPatch(g *directory.Group) *groupPatch {
	gp := &groupPatch{displayName: g.Meta.DisplayName, externalID: g.Spec.External.ID}
	gp.setMembers(g.Spec.MemberIDs)
	return gp
}

func (g *groupPatch) setMembers(ids []string) {
	g.members, g.index = nil, make(map[string]bool, len(ids))
	g.addMembers(ids)
}

func (g *groupPatch) addMembers(ids []string) {
	for _, id := range ids {
		if !g.index[id] {
			g.index[id] = true
			g.members = append(g.members, id)
		}
	}
}

func (g *groupPatch) removeMembers(drop func(id string) bool) {
	g.members = slices.DeleteFunc(g.members, func(id string) bool {
		if drop(id) {
			delete(g.index, id)
			return true
		}
		return false
	})
}

func (g *groupPatch) provisioned() directory.ProvisionedGroup {
	return directory.ProvisionedGroup{DisplayName: g.displayName, ExternalID: g.externalID, MemberIDs: g.members}
}

// changedFields lists the SCIM attributes that differ from stored.
func (g *groupPatch) changedFields(stored *directory.Group) []string {
	var out []string
	if g.displayName != stored.Meta.DisplayName {
		out = append(out, "displayName")
	}
	if g.externalID != stored.Spec.External.ID {
		out = append(out, "externalId")
	}
	if !slices.Equal(g.members, stored.Spec.MemberIDs) {
		out = append(out, "members")
	}
	return out
}

func (g *groupPatch) apply(op string, p patchPath, v json.RawMessage) error {
	switch p.name {
	case "displayname":
		if op == opRemove {
			return badRequest(typeInvalidValue, "displayName is required")
		}
		s, err := decodeString(v, "displayName")
		if err != nil {
			return err
		}
		if s == "" {
			return badRequest(typeInvalidValue, "displayName is required")
		}
		g.displayName = s
	case "externalid":
		if op == opRemove {
			g.externalID = ""
			return nil
		}
		s, err := decodeString(v, "externalId")
		if err != nil {
			return err
		}
		g.externalID = s
	case "members":
		return g.applyMembers(op, p, v)
	default:
		return badRequest(typeInvalidPath, "%s cannot be patched", p.name)
	}
	return nil
}

// applyMembers handles RFC 7644's member forms plus a remove that lists the members to drop in its value, which Microsoft Entra sends without its compliance flag.
func (g *groupPatch) applyMembers(op string, p patchPath, v json.RawMessage) error {
	if p.sub != "" && (op != opRemove || p.filter == nil) {
		return badRequest(typeInvalidPath, "members.%s cannot be patched", p.sub)
	}
	if p.filter != nil {
		if op == opAdd {
			return badRequest(typeInvalidPath, "add takes no value filter")
		}
		g.removeMembers(func(id string) bool { return p.filter.eval(map[string]any{"value": id}) })
		if op == opRemove {
			return nil
		}
	}
	if op == opRemove && v == nil {
		g.setMembers(nil)
		return nil
	}
	in, err := decodeOneOrMany[memberIn](v, "members")
	if err != nil {
		return err
	}
	ids, err := memberIDs(in)
	if err != nil {
		return err
	}
	switch {
	case op == opRemove:
		drop := make(map[string]bool, len(ids))
		for _, id := range ids {
			drop[id] = true
		}
		g.removeMembers(func(id string) bool { return drop[id] })
	case op == opReplace && p.filter == nil:
		g.setMembers(ids)
	default:
		g.addMembers(ids)
	}
	return nil
}
