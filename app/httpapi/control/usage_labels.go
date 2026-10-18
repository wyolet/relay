package control

import (
	"context"
	"log/slog"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/pkg/usage"
)

// Labels a value lacks after the store's event-time slug are filled from the catalog snapshot, then, for user principals (never in the snapshot and never slugged on events), from the users table in one query.

// usernameLookup names users by id in one query; *user.Store satisfies it.
type usernameLookup interface {
	Usernames(ctx context.Context, ids []string) (map[string]string, error)
}

// userNames is d.Users as a lookup, nil without a users store: a nil *user.Store must not become a non-nil interface.
func userNames(d Deps) usernameLookup {
	if d.Users == nil {
		return nil
	}
	return d.Users
}

// lookupUsernames names ids with one query. A failed lookup only costs labels, so it is logged rather than failing the read.
func lookupUsernames(ctx context.Context, users usernameLookup, ids []string) map[string]string {
	if users == nil || len(ids) == 0 {
		return nil
	}
	names, err := users.Usernames(ctx, ids)
	if err != nil {
		slog.Warn("control: usage labels: username lookup failed", "err", err)
		return nil
	}
	return names
}

// catalogLabel names an id value of dim from the current catalog, for values whose events carry no slug (rows older than the slug, host keys). Users are not in the snapshot; lookupUsernames names them.
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

// currentSnapshot is the catalog's snapshot, nil when there is no catalog.
func currentSnapshot(cat *appcatalog.Catalog) *appcatalog.Snapshot {
	if cat == nil {
		return nil
	}
	return cat.Current()
}

// fillFacetLabels labels every facet value the store left unlabeled: from the catalog, then principal_id values from the users table.
func fillFacetLabels(ctx context.Context, cat *appcatalog.Catalog, users usernameLookup, facets map[string][]usage.FacetValue) {
	if snap := currentSnapshot(cat); snap != nil {
		for dim, vals := range facets {
			for i := range vals {
				if vals[i].Label == "" {
					vals[i].Label = catalogLabel(snap, dim, vals[i].Value)
				}
			}
		}
	}
	vals := facets["principal_id"]
	var ids []string
	for _, v := range vals {
		if v.Label == "" && v.Value != "" {
			ids = append(ids, v.Value)
		}
	}
	names := lookupUsernames(ctx, users, ids)
	for i := range vals {
		if vals[i].Label == "" {
			vals[i].Label = names[vals[i].Value]
		}
	}
}
