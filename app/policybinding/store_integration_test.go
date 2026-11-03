//go:build integration

// store_integration_test.go covers the PolicyBinding store against a real Postgres: bound accounts live in a junction whose FK drops an account when it is deleted, and whose unique index holds each account to one binding. Run with: make test-integration.

package policybinding_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
)

type bindingFixture struct {
	ctx      context.Context
	db       storage.DB
	q        *gen.Queries
	accounts *account.Store
	store    *policybinding.Store
	team     *team.Team
	project  *project.Project
	policy   *policy.Policy
}

func setupBindings(t *testing.T, name string) *bindingFixture {
	t.Helper()
	ctx := context.Background()
	db := storagetest.Pool(t)
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	if err := team.NewStore(db).Upsert(ctx, tm); err != nil {
		t.Fatalf("upsert team: %v", err)
	}
	prj := &project.Project{
		Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerTeam, ID: tm.Meta.ID}},
		Spec: project.Spec{TeamID: tm.Meta.ID},
	}
	if err := project.NewStore(db).Upsert(ctx, prj); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	pol := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	if err := policy.NewStore(db).Upsert(ctx, pol); err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	q := gen.New(db)
	return &bindingFixture{ctx: ctx, db: db, q: q, accounts: account.NewStore(q), store: policybinding.NewStore(db), team: tm, project: prj, policy: pol}
}

func (f *bindingFixture) managed(t *testing.T, want *account.Account) string {
	t.Helper()
	id, err := f.accounts.EnsureManaged(f.ctx, want)
	if err != nil {
		t.Fatalf("ensure %s: %v", want.Meta.Name, err)
	}
	return id
}

func (f *bindingFixture) bind(name string, owner meta.Owner, accountIDs ...string) (*policybinding.PolicyBinding, error) {
	b := &policybinding.PolicyBinding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: owner},
		Spec: policybinding.Spec{PolicyID: f.policy.Meta.ID, AccountIDs: accountIDs},
	}
	return b, f.store.Upsert(f.ctx, b)
}

// Accounts of every kind round-trip in order; deleting one, directly or through the host or project it goes with, drops it from the binding and leaves the rest.
func TestIntegration_AccountsRoundTripAndDropOut(t *testing.T) {
	f := setupBindings(t, "drop-out")
	hst := &host.Host{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "pb-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "https://api.example.com"},
	}
	if err := host.NewStore(f.q).Upsert(f.ctx, hst); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	provider := &account.Account{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "acme", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: f.team.Meta.ID}},
		Spec: account.Spec{HostID: hst.Meta.ID},
	}
	if err := f.accounts.Upsert(f.ctx, provider); err != nil {
		t.Fatalf("upsert provider account: %v", err)
	}
	teamAcct := f.managed(t, account.ForTeam(f.team.Meta.ID, f.team.Meta.Name, ""))
	projAcct := f.managed(t, account.ForProject(f.project.Meta.ID, f.project.Meta.Name, ""))
	ids := []string{projAcct, provider.Meta.ID, teamAcct}

	b, err := f.bind("everywhere", meta.Owner{Kind: meta.OwnerSystem}, ids...)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := f.store.Get(f.ctx, b.Meta.ID)
	if err != nil || got == nil || !slices.Equal(got.Spec.AccountIDs, ids) {
		t.Fatalf("Get = %+v (err %v), want accounts %v", got, err, ids)
	}

	if err := host.NewStore(f.q).Delete(f.ctx, hst.Meta.ID); err != nil {
		t.Fatalf("delete host: %v", err)
	}
	if err := project.NewStore(f.db).Delete(f.ctx, f.project.Meta.ID); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	got, err = f.store.Get(f.ctx, b.Meta.ID)
	if err != nil || got == nil || !slices.Equal(got.Spec.AccountIDs, []string{teamAcct}) {
		t.Fatalf("after deletes = %+v (err %v), want only the team account", got, err)
	}
}

// A binding's owner round-trips: a project, a team or the system. Deleting the owning team deletes its binding.
func TestIntegration_OwnersRoundTrip(t *testing.T) {
	f := setupBindings(t, "owners")
	teamAcct := f.managed(t, account.ForTeam(f.team.Meta.ID, f.team.Meta.Name, ""))
	projAcct := f.managed(t, account.ForProject(f.project.Meta.ID, f.project.Meta.Name, ""))
	u := &user.User{ID: meta.NewID(), Username: "owners-alice"}
	if err := user.NewStore(f.db).Upsert(f.ctx, u); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	personal := f.managed(t, account.Personal(u.ID, u.Username))
	for owner, acct := range map[meta.Owner]string{
		{Kind: meta.OwnerSystem}:                         personal,
		{Kind: meta.OwnerTeam, ID: f.team.Meta.ID}:       teamAcct,
		{Kind: meta.OwnerProject, ID: f.project.Meta.ID}: projAcct,
	} {
		b, err := f.bind("owned-by-"+string(owner.Kind), owner, acct)
		if err != nil {
			t.Fatalf("upsert %s-owned: %v", owner.Kind, err)
		}
		got, err := f.store.Get(f.ctx, b.Meta.ID)
		if err != nil || got == nil || got.Meta.Owner != owner {
			t.Fatalf("%s-owned read back as %+v (err %v)", owner.Kind, got, err)
		}
	}
	if err := team.NewStore(f.db).Delete(f.ctx, f.team.Meta.ID); err != nil {
		t.Fatalf("delete team: %v", err)
	}
	all, err := f.store.List(f.ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, b := range all {
		if b.Meta.Owner.Kind != meta.OwnerSystem {
			t.Fatalf("binding %s outlived its team", b.Meta.Name)
		}
	}
}

// An account is held by one binding: a second binding naming it is refused by the store's own index, so two writes racing past the save-time check cannot both land.
func TestIntegration_AccountHeldByOneBinding(t *testing.T) {
	f := setupBindings(t, "one-binding")
	teamAcct := f.managed(t, account.ForTeam(f.team.Meta.ID, f.team.Meta.Name, ""))
	projAcct := f.managed(t, account.ForProject(f.project.Meta.ID, f.project.Meta.Name, ""))
	system := meta.Owner{Kind: meta.OwnerSystem}
	first, err := f.bind("first", system, projAcct)
	if err != nil {
		t.Fatalf("first binding: %v", err)
	}
	if _, err := f.bind("second", system, teamAcct, projAcct); !errors.Is(err, policybinding.ErrAccountBound) {
		t.Fatalf("second binding of the project account: err = %v, want ErrAccountBound", err)
	}
	// Rewriting the holder itself is no conflict.
	first.Spec.AccountIDs = []string{projAcct, teamAcct}
	if err := f.store.Upsert(f.ctx, first); err != nil {
		t.Fatalf("rewrite of the holding binding: %v", err)
	}
}

// A user-owned binding round-trips with its owner and goes when the user is deleted.
func TestIntegration_UserOwnedBinding(t *testing.T) {
	f := setupBindings(t, "own-scope")
	users := user.NewStore(f.db)
	u := &user.User{ID: meta.NewID(), Username: "own-scope-alice"}
	if err := users.Upsert(f.ctx, u); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	personal := f.managed(t, account.Personal(u.ID, u.Username))
	owner := meta.Owner{Kind: meta.OwnerUser, ID: u.ID}
	mine, err := f.bind("alice-own", owner, personal)
	if err != nil {
		t.Fatalf("upsert user-owned: %v", err)
	}
	got, err := f.store.Get(f.ctx, mine.Meta.ID)
	if err != nil || got == nil || got.Meta.Owner != owner {
		t.Fatalf("user-owned read back as %+v (err %v)", got, err)
	}
	if err := users.Delete(f.ctx, u.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if got, err := f.store.Get(f.ctx, mine.Meta.ID); err != nil || got != nil {
		t.Fatalf("user-owned binding after its user was deleted = %v (err %v), want gone", got, err)
	}
}

// Holding finds the one binding that holds an account, and nothing once the account leaves it.
func TestIntegration_Holding(t *testing.T) {
	f := setupBindings(t, "holding")
	teamAcct := f.managed(t, account.ForTeam(f.team.Meta.ID, f.team.Meta.Name, ""))
	projAcct := f.managed(t, account.ForProject(f.project.Meta.ID, f.project.Meta.Name, ""))
	if got, err := f.store.Holding(f.ctx, teamAcct); err != nil || got != nil {
		t.Fatalf("Holding of an unbound account = %+v (err %v), want none", got, err)
	}
	b, err := f.bind("holder", meta.Owner{Kind: meta.OwnerSystem}, projAcct, teamAcct)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	got, err := f.store.Holding(f.ctx, teamAcct)
	if err != nil || got == nil || got.Meta.ID != b.Meta.ID || !slices.Equal(got.Spec.AccountIDs, []string{projAcct, teamAcct}) {
		t.Fatalf("Holding = %+v (err %v), want %s with both accounts", got, err, b.Meta.ID)
	}
	b.Spec.AccountIDs = []string{projAcct}
	if err := f.store.Upsert(f.ctx, b); err != nil {
		t.Fatalf("drop the team account: %v", err)
	}
	if got, err := f.store.Holding(f.ctx, teamAcct); err != nil || got != nil {
		t.Fatalf("Holding after the account left = %+v (err %v), want none", got, err)
	}
}
