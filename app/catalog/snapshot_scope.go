package catalog

import "github.com/wyolet/relay/app/meta"

// global is the outermost scope every chain ends in.
var global = meta.Owner{Kind: meta.OwnerSystem}

// ScopeChain returns the scopes a row with this owner lives in, most
// specific first. A nil owner is a global resource (settings, list calls).
// An owner whose project or team is absent collapses to the global scope,
// which is what lets an admin still reach the row.
func (s *Snapshot) ScopeChain(o *meta.Owner) []meta.Owner {
	if o == nil {
		return []meta.Owner{global}
	}
	switch o.Kind {
	case meta.OwnerProject:
		if p, ok := s.projectsByID[o.ID]; ok {
			return []meta.Owner{*o, {Kind: meta.OwnerTeam, ID: p.Spec.TeamID}, global}
		}
	case meta.OwnerTeam:
		if _, ok := s.teamsByID[o.ID]; ok {
			return []meta.Owner{*o, global}
		}
	}
	return []meta.Owner{global}
}

// ScopeChainFor is ScopeChain for a row that is itself a scope. A Team and
// a Project define the scope they live in; their owner does not name it (a
// Team is system-owned, a Project team-owned), so a binding at the scope a
// row defines would otherwise never reach that row. Every other kind
// delegates to ScopeChain.
func (s *Snapshot) ScopeChainFor(kind, id string, o *meta.Owner) []meta.Owner {
	if id == "" {
		return s.ScopeChain(o)
	}
	switch kind {
	case "team":
		return []meta.Owner{{Kind: meta.OwnerTeam, ID: id}, global}
	case "project":
		if p, ok := s.projectsByID[id]; ok {
			return []meta.Owner{
				{Kind: meta.OwnerProject, ID: id},
				{Kind: meta.OwnerTeam, ID: p.Spec.TeamID},
				global,
			}
		}
		// Not in the snapshot yet (a create) or no longer in it (disabled):
		// the owner still names the team, and that is the scope the write
		// has to be authorized at.
		return s.ScopeChain(o)
	}
	return s.ScopeChain(o)
}
