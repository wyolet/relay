package control

import (
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/pkg/usage"
)

// catalogLabel names an id value of dim from the current catalog, for values whose events carry no slug (rows older than the slug, host keys). Users are not in the snapshot, so a user principal id stays unlabeled.
func catalogLabel(snap *appcatalog.Snapshot, dim, id string) string {
	if id == "" {
		return ""
	}
	switch dim {
	case "model_id":
		if m, ok := snap.Model(id); ok {
			return m.Meta.Name
		}
	case "host_id":
		if h, ok := snap.Host(id); ok {
			return h.Meta.Name
		}
	case "policy_id":
		if p, ok := snap.Policy(id); ok {
			return p.Meta.Name
		}
	case "project_id":
		if p, ok := snap.Project(id); ok {
			return p.Meta.Name
		}
	case "team_id":
		if t, ok := snap.Team(id); ok {
			return t.Meta.Name
		}
	case "principal_id":
		if sa, ok := snap.ServiceAccount(id); ok {
			return sa.Meta.Name
		}
	case "relay_key_hash":
		if k, _ := snap.KeyByHash(id); k != nil {
			return k.Meta.Name
		}
	case "credential_id":
		if k, ok := snap.Key(id); ok {
			return k.Meta.Name
		}
	case "host_key_id":
		if hk, ok := snap.HostKey(id); ok {
			return hk.Meta.Name
		}
	}
	return ""
}

// fillFacetLabels sets a catalog label on every facet value the store left unlabeled.
func fillFacetLabels(cat *appcatalog.Catalog, facets map[string][]usage.FacetValue) {
	if cat == nil {
		return
	}
	snap := cat.Current()
	if snap == nil {
		return
	}
	for dim, vals := range facets {
		for i := range vals {
			if vals[i].Label == "" {
				vals[i].Label = catalogLabel(snap, dim, vals[i].Value)
			}
		}
	}
}
