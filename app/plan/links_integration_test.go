//go:build integration

package plan_test

import (
	"context"
	"testing"
	"time"

	"github.com/wyolet/relay/app/account"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/engine/cell"
	"github.com/wyolet/relay/pkg/meta"
)

// loadPlans runs a plan engine over the stores, as boot does, and returns the view it published.
func loadPlans(t *testing.T, stores *appcatalog.Stores) *engine.View {
	t.Helper()
	cells := cell.NewRegistry(cell.Options{IdleTTL: time.Minute})
	e := engine.New(engine.Options{Cells: cells})
	plan.Register(e, cells, plan.Sources{
		Providers: stores.Provider, Hosts: stores.Host, Models: stores.Model, Bindings: stores.Binding, Pricings: stores.Pricing,
		Policies: stores.Policy, PolicyBindings: stores.PolicyBinding, RateLimits: stores.RateLimit, Budgets: stores.Budget,
		Pools: stores.Pool, Accounts: stores.Account, Credentials: stores.HostKey, Teams: stores.Team, Projects: stores.Project,
		ServiceAccounts: stores.ServiceAccount, Users: stores.Users, Keys: stores.Key, RoleBindings: stores.RoleBinding,
	})
	if err := e.Load(context.Background()); err != nil {
		t.Fatalf("load plans: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e.View()
}

// Deleting the only pool a policy links narrows the principal to nothing after a reload from the store: the policy keeps the pool's id, so it never falls back to the credentials the principal's scopes own.
func TestIntegration_DeletedPoolNarrowsAfterReload(t *testing.T) {
	ctx := context.Background()
	_, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: storagetest.Pool(t)})
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	system := meta.Owner{Kind: meta.OwnerSystem}
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	hst := &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: "links-host", Owner: system}, Spec: host.Spec{BaseURL: "https://api.example.com"}}
	must("upsert host", stores.Host.Upsert(ctx, hst))
	t.Setenv("LINKS_KEY_VALUE", "sk-links")
	org := &account.Account{Meta: meta.Metadata{ID: meta.NewID(), Name: "links-org", Owner: system}, Spec: account.Spec{HostID: hst.Meta.ID}}
	must("upsert account", stores.Account.Upsert(ctx, org))
	cred := &hostkey.HostKey{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "links-key", Owner: system},
		Spec: hostkey.Spec{HostID: hst.Meta.ID, AccountID: org.Meta.ID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "LINKS_KEY_VALUE"}},
	}
	must("upsert credential", stores.HostKey.Upsert(ctx, cred))
	pl := &pool.Pool{Meta: meta.Metadata{ID: meta.NewID(), Name: "links-pool", Owner: system}, Spec: pool.Spec{HostID: hst.Meta.ID, CredentialIDs: []string{cred.Meta.ID}}}
	must("upsert pool", stores.Pool.Upsert(ctx, pl))
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "links-team", Owner: system}}
	must("upsert team", stores.Team.Upsert(ctx, tm))
	prj := &project.Project{Meta: meta.Metadata{ID: meta.NewID(), Name: "links-project", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: tm.Meta.ID}}, Spec: project.Spec{TeamID: tm.Meta.ID}}
	must("upsert project", stores.Project.Upsert(ctx, prj))
	sa := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "links-sa"}, Spec: serviceaccount.Spec{ProjectID: prj.Meta.ID}}
	sa.StampOwner()
	must("upsert service account", stores.ServiceAccount.Upsert(ctx, sa))
	pol := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "links-policy", Owner: meta.Owner{Kind: meta.OwnerProject, ID: prj.Meta.ID}}, Spec: policy.Spec{PoolIDs: []string{pl.Meta.ID}}}
	must("upsert policy", stores.Policy.Upsert(ctx, pol))
	saAccount, err := stores.Account.EnsureManaged(ctx, account.ForServiceAccount(sa.Meta.ID, sa.Meta.Name, "", prj.Meta.ID))
	must("ensure the service account's account", err)
	must("upsert binding", stores.PolicyBinding.Upsert(ctx, &policybinding.PolicyBinding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "links-binding", Owner: meta.Owner{Kind: meta.OwnerProject, ID: prj.Meta.ID}},
		Spec: policybinding.Spec{PolicyID: pol.Meta.ID, AccountIDs: []string{saAccount}},
	}))
	principal := "account:" + saAccount

	if cp := engine.Get[plan.CallerPlan](loadPlans(t, stores), principal); cp == nil || len(cp.Pools) != 1 || len(cp.Pools[0].Slots) != 1 {
		t.Fatalf("before the delete: caller plan %+v, want the linked pool with its credential", cp)
	}

	must("delete pool", stores.Pool.Delete(ctx, pl.Meta.ID))
	v := loadPlans(t, stores)
	cp := engine.Get[plan.CallerPlan](v, principal)
	if cp == nil || cp.Refused != nil {
		t.Fatalf("after the delete: caller plan %v, want one that serves", cp)
	}
	if len(cp.Pools) != 0 {
		t.Fatalf("after the delete: %d pools, want none (the system-owned credential must not be reached by ownership)", len(cp.Pools))
	}
	missing := false
	for _, is := range v.Issues(engine.Ref{Kind: plan.KindPolicy, ID: pol.Meta.ID}) {
		missing = missing || is.Code == plan.IssueRefMissing
	}
	if !missing {
		t.Fatal("the policy linking the deleted pool has no ref_missing issue")
	}
}
