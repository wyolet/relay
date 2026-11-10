package apply

import (
	"context"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/rolebinding"
	authmanifest "github.com/wyolet/relay/auth/manifest"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/pkg/slug"
)

// planCallerAccounts names the caller accounts the run's rows call for and no stored row holds yet: a declared team's, project's or service account's own, a declared user's personal account, and a member account per user a declared role binding names in a team or project. Relay's account writer would make them once those rows land; planning them here lets a binding or key in the same bundle name them, and the write of that binding or key makes them first.
func (b *builder) planCallerAccounts(teamDocs []*manifest.TeamDTO, projDocs []*manifest.ProjectDTO, saDocs []*manifest.ServiceAccountDTO, userDocs []*authmanifest.Document, rbDocs []*manifest.RoleBindingDTO) {
	held := make(map[string]bool, len(b.rows.Accounts))
	for _, a := range b.rows.Accounts {
		if k := a.ManagedKey(); k != "" {
			held[k] = true
		}
	}
	plan := func(want *account.Account) {
		place := want.ManagedKey()
		if place == "" || held[place] {
			return
		}
		held[place] = true
		want.Meta.ID = ids.New()
		want.Meta.Name = slug.Unique(want.Meta.Name, func(c string) bool { _, taken := b.idx.Accounts[c]; return taken })
		b.idx.Accounts[want.Meta.Name] = want.Meta.ID
		if b.declared == nil {
			b.declared = map[string]any{}
		}
		b.declared[want.Meta.ID] = want
		if b.callerAccounts == nil {
			b.callerAccounts = map[string]*account.Account{}
		}
		b.callerAccounts[want.Meta.ID] = want
	}
	for _, d := range teamDocs {
		plan(account.ForTeam(b.idx.Teams[d.Metadata.Name], d.Metadata.Name, d.Metadata.DisplayName))
	}
	for _, d := range projDocs {
		plan(account.ForProject(b.idx.Projects[d.Metadata.Name], d.Metadata.Name, d.Metadata.DisplayName))
	}
	for _, d := range saDocs {
		if projectID := b.idx.Projects[d.Spec.Project]; projectID != "" {
			plan(account.ForServiceAccount(b.idx.ServiceAccounts[d.Metadata.Name], d.Metadata.Name, d.Metadata.DisplayName, projectID))
		}
	}
	for _, d := range userDocs {
		plan(account.Personal(b.idx.Users[d.Name()], d.Name()))
	}
	for _, d := range rbDocs {
		if d.Spec.Enabled != nil && !*d.Spec.Enabled {
			continue
		}
		scope, ok := b.scopeOf(d.Spec.Scope)
		if !ok {
			continue
		}
		for _, s := range d.Spec.Subjects {
			if userID := b.idx.Users[s.Name]; s.Kind == string(rolebinding.SubjectUser) && userID != "" {
				want := account.Member(userID, s.Name, scope.owner, scope.name, "")
				want.FollowsRoleBindings = true
				plan(want)
			}
		}
	}
}

type namedScope struct {
	owner meta.Owner
	name  string
}

// scopeOf resolves a role binding document's team or project scope; ok is false for any other scope or one that names nothing.
func (b *builder) scopeOf(o manifest.WireOwner) (namedScope, bool) {
	names := map[meta.OwnerKind]map[string]string{meta.OwnerTeam: b.idx.Teams, meta.OwnerProject: b.idx.Projects}[o.Kind]
	if names == nil {
		return namedScope{}, false
	}
	for name, id := range names {
		if name == o.Name || (o.ID != "" && id == o.ID) {
			return namedScope{owner: meta.Owner{Kind: o.Kind, ID: id}, name: name}, true
		}
	}
	return namedScope{}, false
}

// ensureCallerAccounts makes each planned caller account among ids through the account writer and returns ids with the stored ids in their place; another writer may have made one first under its own id.
func (b *builder) ensureCallerAccounts(ctx context.Context, accountIDs []string) ([]string, error) {
	out := append([]string(nil), accountIDs...)
	for i, id := range accountIDs {
		if want := b.callerAccounts[id]; want != nil {
			got, err := b.opts.Stores.Account.EnsureManaged(ctx, want)
			if err != nil {
				return nil, err
			}
			out[i] = got
		}
	}
	return out, nil
}

// upsertPolicyBinding writes the binding once the caller accounts it names exist.
func (b *builder) upsertPolicyBinding(ctx context.Context, pb *policybinding.PolicyBinding) error {
	accountIDs, err := b.ensureCallerAccounts(ctx, pb.Spec.AccountIDs)
	if err != nil {
		return err
	}
	pb.Spec.AccountIDs = accountIDs
	return b.opts.Stores.PolicyBinding.Upsert(ctx, pb)
}

// upsertKey writes the key once the caller account it runs in exists.
func (b *builder) upsertKey(ctx context.Context, k *key.Key) error {
	if k.Spec.AccountID != "" {
		accountIDs, err := b.ensureCallerAccounts(ctx, []string{k.Spec.AccountID})
		if err != nil {
			return err
		}
		k.Spec.AccountID = accountIDs[0]
	}
	return b.opts.Stores.Key.Upsert(ctx, k)
}
