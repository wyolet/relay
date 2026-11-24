package manifest

import (
	"fmt"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/rbac"
)

// checkPrunedScopes refuses a RoleBinding document scoped to, or a ServiceAccount document owned by, a team or project the plan prunes, or a project of a pruned team: the store deletes them with their scope, so they would be written and then lost. It refuses pruning a team that still owns a stored service account the plan keeps: the store refuses that team's delete, which would stop the apply after its earlier writes.
func (p *planner) checkPrunedScopes() error {
	gone := map[rbac.Scope]bool{}
	deleted := map[string]bool{}
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
			case KindServiceAccount:
				deleted[e.ID] = true
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
		if gone[rbac.Scope(sa.Meta.Owner)] {
			return docError(d, "spec."+sa.Meta.Owner.Kind, ErrScopePruned)
		}
	}
	for _, sa := range p.rows.serviceAccounts {
		if o := sa.Meta.Owner; o.Kind == directory.OwnerTeam && gone[rbac.Scope(o)] && !deleted[sa.Meta.ID] {
			return fmt.Errorf("manifest: service account %q: %w", sa.Meta.Name, ErrTeamOwnsServiceAccount)
		}
	}
	return nil
}
