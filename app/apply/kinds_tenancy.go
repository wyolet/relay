package apply

import (
	"context"

	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
)

func (b *builder) planTenancy(ctx context.Context, teamDocs []*manifest.TeamDTO, projDocs []*manifest.ProjectDTO) error {
	s := b.opts.Stores
	if err := planKind(ctx, b, kindWiring[manifest.TeamDTO, team.Team]{
		Kind: "Team", Docs: teamDocs, Names: b.idx.Teams, Rows: b.rows.Teams,
		To: manifest.ToTeam, Meta: func(t *team.Team) *meta.Metadata { return &t.Meta },
		Upsert: s.Team.Upsert, Delete: s.Team.Delete,
	}); err != nil {
		return err
	}
	if err := planKind(ctx, b, kindWiring[manifest.ProjectDTO, project.Project]{
		Kind: "Project", Docs: projDocs, Names: b.idx.Projects, Rows: b.rows.Projects,
		To: manifest.ToProject, Meta: func(p *project.Project) *meta.Metadata { return &p.Meta },
		Upsert: s.Project.Upsert, Delete: s.Project.Delete,
		Check: refsFor(b, refcheck.Checker.Project),
	}); err != nil {
		return err
	}
	return nil
}

func (b *builder) planServiceAccountsAndKeys(ctx context.Context, saDocs []*manifest.ServiceAccountDTO, keyDocs []*manifest.KeyDTO) error {
	s := b.opts.Stores
	if err := planKind(ctx, b, kindWiring[manifest.ServiceAccountDTO, serviceaccount.ServiceAccount]{
		Kind: "ServiceAccount", Docs: saDocs, Names: b.idx.ServiceAccounts, Rows: b.rows.ServiceAccounts,
		To: manifest.ToServiceAccount, Meta: func(sa *serviceaccount.ServiceAccount) *meta.Metadata { return &sa.Meta },
		Upsert: s.ServiceAccount.Upsert, Delete: s.ServiceAccount.Delete,
		Check: refsFor(b, refcheck.Checker.ServiceAccount),
	}); err != nil {
		return err
	}
	if err := planKind(ctx, b, kindWiring[manifest.KeyDTO, key.Key]{
		Kind: "Key", Docs: keyDocs, Names: b.idx.Keys, Rows: b.rows.Keys,
		To: manifest.ToKey, Meta: func(k *key.Key) *meta.Metadata { return &k.Meta },
		Upsert: s.Key.Upsert, Delete: s.Key.Delete,
		Check: refsFor(b, refcheck.Checker.Key),
		Keep:  keepKeyServerFields,
	}); err != nil {
		return err
	}
	return nil
}

// keepKeyServerFields carries the rotation state a manifest cannot author
// (the fields are yaml:"-") onto the declared key, so the diff ignores it and
// an update keeps it.
func keepKeyServerFields(prev, next *key.Key) {
	next.Spec.PreviousKeyHash = prev.Spec.PreviousKeyHash
	next.Spec.GraceUntil = prev.Spec.GraceUntil
}

// checkPrunedTenancy refuses pruning a team or project with rows still under
// it, as the control API's delete does. Rows this run prunes too are deleted
// explicitly, children first, so they are not in the way.
func (b *builder) checkPrunedTenancy() error {
	pruned := map[string]bool{}
	for _, kind := range b.deletes {
		for _, e := range kind {
			pruned[e.Kind+"/"+e.ID] = true
		}
	}
	gone := func(kind, id string) bool { return pruned[kind+"/"+id] }
	under := project.Rows{
		ServiceAccounts: b.rows.ServiceAccounts, Keys: b.rows.Keys, Policies: b.rows.Policies,
		HostKeys: b.rows.HostKeys, RateLimits: b.rows.RateLimits, PolicyBindings: b.rows.PolicyBindings,
	}
	for _, kind := range b.deletes {
		for _, e := range kind {
			var deps []string
			switch e.Kind {
			case "Team":
				deps = project.OfTeam(e.ID, b.rows.Projects, gone)
			case "Project":
				deps = under.Dependents(e.ID, gone)
			}
			if len(deps) > 0 {
				return &InvalidError{Kind: e.Kind, Name: e.Name, Err: &project.DependentsError{Rows: deps}}
			}
		}
	}
	return nil
}
