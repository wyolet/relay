package apply

import (
	"context"
	"errors"

	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	authmanifest "github.com/wyolet/relay/auth/manifest"
)

func (b *builder) planTenancy(ctx context.Context, teamDocs []*manifest.TeamDTO, projDocs []*manifest.ProjectDTO) error {
	if err := planKind(ctx, b, kindWiring[manifest.TeamDTO, team.Team]{
		Kind: "Team", Docs: teamDocs, Names: b.idx.Teams, Rows: b.rows.Teams,
		To: manifest.ToTeam, Meta: func(t *team.Team) *meta.Metadata { return &t.Meta },
		Upsert: throughDirectory(b, converted((*team.Team).ToDirectory)), Delete: deleteThroughDirectory(b, "Team"),
	}); err != nil {
		return err
	}
	if err := planKind(ctx, b, kindWiring[manifest.ProjectDTO, project.Project]{
		Kind: "Project", Docs: projDocs, Names: b.idx.Projects, Rows: b.rows.Projects,
		To: manifest.ToProject, Meta: func(p *project.Project) *meta.Metadata { return &p.Meta },
		Upsert: throughDirectory(b, converted((*project.Project).ToDirectory)), Delete: deleteThroughDirectory(b, "Project"),
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
		Upsert: throughDirectory(b, converted((*serviceaccount.ServiceAccount).ToDirectory)), Delete: deleteThroughDirectory(b, "ServiceAccount"),
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

// checkPrunedTenancy refuses pruning a team, project or service account with rows still under it, as the control API's delete does. Rows this run prunes too are deleted explicitly, children first, so they are not in the way.
func (b *builder) checkPrunedTenancy(ctx context.Context) error {
	pruned := map[string]bool{}
	for _, kind := range b.deletes {
		for _, e := range kind {
			pruned[e.Kind+"/"+e.ID] = true
		}
	}
	gone := func(kind, id string) bool { return pruned[kind+"/"+id] }
	visible := func(kind string, m meta.Metadata) bool { return b.visible(ctx, kind, m.ID, m.Owner) }
	under := project.Rows{
		ServiceAccounts: b.rows.ServiceAccounts, Keys: b.rows.Keys, Policies: b.rows.Policies,
		HostKeys: b.rows.HostKeys, RateLimits: b.rows.RateLimits, PolicyBindings: b.rows.PolicyBindings,
	}
	for _, kind := range b.deletes {
		for _, e := range kind {
			var deps []string
			var hidden int
			switch e.Kind {
			case "Team":
				deps, hidden = project.OfTeam(e.ID, b.rows.Projects, gone, visible)
			case "Project":
				deps, hidden = under.Dependents(e.ID, gone, visible)
			case "ServiceAccount":
				deps, hidden = b.serviceAccountDependents(e.ID, gone, visible)
			}
			if len(deps) > 0 || hidden > 0 {
				return &InvalidError{Kind: e.Kind, Name: e.Name, Err: &project.DependentsError{Rows: deps, Hidden: hidden}}
			}
		}
	}
	return b.checkDocsAtPrunedScopes(ctx)
}

// serviceAccountDependents names, as "Kind/name", the rows deleting service account id would change through the foreign keys: its keys, which go with it, and the bindings that name it as a subject, which lose it. Bindings are read as this run leaves them, since its writes land before its deletes.
func (b *builder) serviceAccountDependents(id string, gone project.Gone, visible project.Visible) (named []string, hidden int) {
	add := func(kind string, m meta.Metadata) {
		switch {
		case gone(kind, m.ID):
		case !visible(kind, m):
			hidden++
		default:
			named = append(named, kind+"/"+m.Name)
		}
	}
	for _, k := range b.rows.Keys {
		if k.Spec.Principal.Kind == key.PrincipalServiceAccount && k.Spec.Principal.ID == id {
			add("Key", k.Meta)
		}
	}
	account := rolebinding.Subject{Kind: rolebinding.SubjectServiceAccount, ID: id}
	subject := account.Key()
	for _, rb := range afterRun(b, b.rows.RoleBindings, func(x *rolebinding.RoleBinding) *meta.Metadata { return &x.Meta }) {
		if namesSubject(rb.Spec.Subjects, subject) {
			add("RoleBinding", rb.Meta)
		}
	}
	for _, pb := range afterRun(b, b.rows.PolicyBindings, func(x *policybinding.PolicyBinding) *meta.Metadata { return &x.Meta }) {
		if namesSubject(pb.Spec.Subjects, subject) {
			add("PolicyBinding", pb.Meta)
		}
	}
	return named, hidden
}

// afterRun is rows as this run leaves them: a declared row replaces its stored one, and rows the run creates are added in plan order.
func afterRun[T any](b *builder, rows []*T, metaOf func(*T) *meta.Metadata) []*T {
	out := make([]*T, 0, len(rows))
	for _, r := range rows {
		if d, ok := b.declared[metaOf(r).ID].(*T); ok {
			r = d
		}
		out = append(out, r)
	}
	for _, e := range b.entries {
		if d, ok := b.declared[e.ID].(*T); ok && e.Action == ActionCreate {
			out = append(out, d)
		}
	}
	return out
}

func namesSubject(subjects []rolebinding.Subject, key string) bool {
	for i := range subjects {
		if subjects[i].Key() == key {
			return true
		}
	}
	return false
}

// errPrincipalPruned refuses a Key document whose service account this run prunes: the key goes with the account, so it would be written and then lost.
var errPrincipalPruned = errors.New("its service account is pruned in the same apply, which deletes it too; drop the document or keep the service account")

// errOwnerPruned refuses a Policy, HostKey or RateLimit document owned by a project this run prunes, directly or with its team. Their owner lives only in metadata, so no delete takes them along: they would be written and left owned by a project that no longer exists.
var errOwnerPruned = errors.New("its project or the project's team is pruned in the same apply, which would leave it owned by a project that no longer exists; drop the document or keep its team and project")

// checkDocsAtPrunedScopes refuses a document this run writes into a team or project it also prunes, or into a project of a pruned team, and a Key of a service account it prunes. A Project, ServiceAccount, Key, RoleBinding or PolicyBinding goes with the delete, so it would be written and then lost; a Policy, HostKey or RateLimit would outlive its owner.
func (b *builder) checkDocsAtPrunedScopes(ctx context.Context) error {
	gone := map[meta.Owner]bool{}
	goneAccounts := map[string]bool{}
	for _, kind := range b.deletes {
		for _, e := range kind {
			switch e.Kind {
			case "ServiceAccount":
				goneAccounts[e.ID] = true
			case "Team":
				gone[meta.Owner{Kind: meta.OwnerTeam, ID: e.ID}] = true
				for _, p := range b.rows.Projects {
					if p.Spec.TeamID == e.ID {
						gone[meta.Owner{Kind: meta.OwnerProject, ID: p.Meta.ID}] = true
					}
				}
			case "Project":
				gone[meta.Owner{Kind: meta.OwnerProject, ID: e.ID}] = true
			}
		}
	}
	if len(gone) == 0 && len(goneAccounts) == 0 {
		return nil
	}
	serviceAccount := lookup(b, b.rows.ServiceAccounts, func(x *serviceaccount.ServiceAccount) *meta.Metadata { return &x.Meta })
	for _, e := range b.entries {
		var err error
		switch e.Kind {
		case "Project", "RoleBinding", "ServiceAccount":
			// The owner mirrors the team, the scope, and the project.
			if gone[e.owner] {
				err = authmanifest.ErrScopePruned
			}
		case "Policy", "HostKey", "RateLimit":
			if gone[e.owner] {
				err = errOwnerPruned
			}
		case "Key":
			// The key goes with its service account, so its scope is the account's project, whatever the document's owner says.
			k, _ := b.declared[e.ID].(*key.Key)
			if k != nil && k.Spec.Principal.Kind == key.PrincipalServiceAccount {
				if sa := serviceAccount(ctx, k.Spec.Principal.ID); sa != nil && gone[meta.Owner{Kind: meta.OwnerProject, ID: sa.Spec.ProjectID}] {
					err = authmanifest.ErrScopePruned
				} else if goneAccounts[k.Spec.Principal.ID] {
					err = errPrincipalPruned
				}
			}
		case "PolicyBinding":
			if pb, _ := b.declared[e.ID].(*policybinding.PolicyBinding); pb != nil && gone[meta.Owner{Kind: meta.OwnerProject, ID: pb.Spec.ProjectID}] {
				err = authmanifest.ErrScopePruned
			}
		}
		if err != nil {
			return &InvalidError{Kind: e.Kind, Name: e.Name, Err: err}
		}
	}
	return nil
}
