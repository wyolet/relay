package manifest

import (
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/rbac"
)

// checkPrunedScopes refuses a RoleBinding document scoped to, or a ServiceAccount document in, a team or project the plan prunes, or a project of a pruned team: the store deletes them with their scope, so they would be written and then lost.
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
	for _, d := range p.docs[KindServiceAccount] {
		sa, err := p.reg.ToServiceAccount(d, p.idx)
		if err != nil {
			return err
		}
		if gone[rbac.Scope{Kind: directory.OwnerProject, ID: sa.Spec.ProjectID}] {
			return docError(d, "spec.project", ErrScopePruned)
		}
	}
	return nil
}
