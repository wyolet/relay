package manifest

import (
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/rbac"
)

// checkPrunedScopes refuses a RoleBinding document scoped to a team or project the plan prunes, or to a project of a pruned team: the store deletes a scope's bindings with it, so the binding would be written and then lost.
func (p *planner) checkPrunedScopes() error {
	gone := map[rbac.Scope]bool{}
	for _, kind := range p.deletes {
		for _, e := range kind {
			if e.Action != ActionDelete {
				continue
			}
			switch e.Kind {
			case KindTeam:
				gone[rbac.Scope{Kind: directory.OwnerTeam, ID: e.ID}] = true
				for _, pr := range p.rows.projects {
					if pr.Spec.TeamID == e.ID {
						gone[rbac.Scope{Kind: directory.OwnerProject, ID: pr.Meta.ID}] = true
					}
				}
			case KindProject:
				gone[rbac.Scope{Kind: directory.OwnerProject, ID: e.ID}] = true
			}
		}
	}
	if len(gone) == 0 {
		return nil
	}
	for _, d := range p.docs[KindRoleBinding] {
		b, err := p.reg.ToRoleBinding(d, p.idx)
		if err != nil {
			return err
		}
		if gone[b.Spec.Scope] {
			return docError(d, "spec.scope", ErrScopePruned)
		}
	}
	return nil
}
