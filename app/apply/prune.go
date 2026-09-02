package apply

import (
	"fmt"
	"strings"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/role"
)

// labelSelector is an equality-only label match: every pair must be present
// on the row. Empty selector matches nothing (prune requires one).
type labelSelector map[string]string

// parseSelector reads "k=v,k2=v2". An empty string yields an empty selector.
func parseSelector(raw string) (labelSelector, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	sel := labelSelector{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" {
			return nil, fmt.Errorf("apply: malformed selector term %q (want key=value)", part)
		}
		sel[k] = v
	}
	if len(sel) == 0 {
		return nil, fmt.Errorf("apply: selector %q names no terms", raw)
	}
	return sel, nil
}

func (s labelSelector) matches(labels map[string]string) bool {
	if len(s) == 0 {
		return false
	}
	for k, v := range s {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// tenantKinds are authored by tenants whatever their owner says: a Team,
// Group or Role names a scope or a grant and is system-owned by rule, and a
// RoleBinding's owner mirrors its scope (a global binding is system-owned).
// Membership is therefore decided by kind, not by provenance.
var tenantKinds = map[string]bool{
	"Team": true, "Group": true, "Role": true, "RoleBinding": true,
}

// prunable reports whether a row's provenance allows apply to delete it.
// System, provider, and host rows belong to the catalog and the relay
// itself; a manifest that omits them is not asking for their removal.
// Built-in Roles are relay's own rows and stay out regardless of kind.
func prunable(kind, name string, o meta.Owner) bool {
	if kind == "Role" && role.IsBuiltin(name) {
		return false
	}
	if tenantKinds[kind] {
		return true
	}
	switch o.Kind {
	case meta.OwnerUser:
		// An empty id names no user: rows shipped by a catalog before owners
		// carried ids are the operator's, and no manifest declares them.
		return o.ID != ""
	case meta.OwnerTeam, meta.OwnerProject:
		return true
	default:
		return false
	}
}
