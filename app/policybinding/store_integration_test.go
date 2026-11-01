//go:build integration

// store_integration_test.go covers the PolicyBinding store against a real Postgres: subjects live in a junction whose FK columns drop a subject when the row it names is deleted. Run with: make test-integration.

package policybinding_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/secret"
)

func TestIntegration_SubjectsOfEveryKindRoundTripAndDropOut(t *testing.T) {
	ctx := context.Background()
	db := storagetest.Pool(t)
	q := gen.New(db)

	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "ml", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	if err := team.NewStore(db).Upsert(ctx, tm); err != nil {
		t.Fatalf("upsert team: %v", err)
	}
	prj := &project.Project{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "eval", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: tm.Meta.ID}},
		Spec: project.Spec{TeamID: tm.Meta.ID},
	}
	if err := project.NewStore(db).Upsert(ctx, prj); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	hst := &host.Host{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "pb-host", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "https://api.example.com"},
	}
	if err := host.NewStore(q).Upsert(ctx, hst); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	reg := secret.NewRegistry()
	reg.Register(secret.KindEnv, secret.EnvResolver{})
	keys := hostkey.NewStore(db, reg, nil)
	hk := &hostkey.HostKey{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "pb-key", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: hostkey.Spec{HostID: hst.Meta.ID, ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "ACME_API_KEY"}},
	}
	if err := keys.Upsert(ctx, hk); err != nil {
		t.Fatalf("upsert host key: %v", err)
	}
	acct := &account.Account{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "acme", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: tm.Meta.ID}},
		Spec: account.Spec{HostID: hst.Meta.ID},
	}
	if err := account.NewStore(q).Upsert(ctx, acct); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	pol := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "contractors", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	if err := policy.NewStore(db).Upsert(ctx, pol); err != nil {
		t.Fatalf("upsert policy: %v", err)
	}

	store := policybinding.NewStore(db)
	subjects := []rolebinding.Subject{
		{Kind: policybinding.SubjectHost, ID: hst.Meta.ID},
		{Kind: policybinding.SubjectCredential, ID: hk.Meta.ID},
		{Kind: policybinding.SubjectAccount, ID: acct.Meta.ID},
		{Kind: policybinding.SubjectTeam, ID: tm.Meta.ID},
		{Kind: policybinding.SubjectProject, ID: prj.Meta.ID},
		{Kind: rolebinding.SubjectGroup, Name: "system:authenticated"},
	}
	b := &policybinding.PolicyBinding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "contractors-everywhere"},
		Spec: policybinding.Spec{ProjectID: prj.Meta.ID, PolicyID: pol.Meta.ID, Subjects: subjects},
	}
	if err := store.Upsert(ctx, b); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := store.Get(ctx, b.Meta.ID)
	if err != nil || got == nil {
		t.Fatalf("Get: %v (row %v)", err, got)
	}
	if !reflect.DeepEqual(got.Spec.Subjects, subjects) {
		t.Fatalf("subjects = %+v, want %+v", got.Spec.Subjects, subjects)
	}

	// Deleting the credential and the host (which takes its account with it) removes them from the binding; the other subjects stay.
	if err := keys.Delete(ctx, hk.Meta.ID); err != nil {
		t.Fatalf("delete host key: %v", err)
	}
	if err := host.NewStore(q).Delete(ctx, hst.Meta.ID); err != nil {
		t.Fatalf("delete host: %v", err)
	}
	got, err = store.Get(ctx, b.Meta.ID)
	if err != nil || got == nil {
		t.Fatalf("Get after deletes: %v (row %v)", err, got)
	}
	if want := subjects[3:]; !reflect.DeepEqual(got.Spec.Subjects, want) {
		t.Fatalf("subjects after deletes = %+v, want %+v", got.Spec.Subjects, want)
	}
}

// A binding's owner round-trips through its columns: a project, a team or the system. Deleting the owning team deletes its binding.
func TestIntegration_OwnersRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := storagetest.Pool(t)
	teams := team.NewStore(db)
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "owners", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	if err := teams.Upsert(ctx, tm); err != nil {
		t.Fatalf("upsert team: %v", err)
	}
	prj := &project.Project{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "owners", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: tm.Meta.ID}},
		Spec: project.Spec{TeamID: tm.Meta.ID},
	}
	if err := project.NewStore(db).Upsert(ctx, prj); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	pol := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "owners", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	if err := policy.NewStore(db).Upsert(ctx, pol); err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	store := policybinding.NewStore(db)
	for i, owner := range []meta.Owner{
		{Kind: meta.OwnerSystem},
		{Kind: meta.OwnerTeam, ID: tm.Meta.ID},
		{Kind: meta.OwnerProject, ID: prj.Meta.ID},
	} {
		b := &policybinding.PolicyBinding{
			Meta: meta.Metadata{ID: meta.NewID(), Name: "owned-by-" + string(owner.Kind), Owner: owner},
			Spec: policybinding.Spec{PolicyID: pol.Meta.ID, Subjects: []rolebinding.Subject{{Kind: policybinding.SubjectProject, ID: prj.Meta.ID}}},
		}
		if i > 0 {
			// One binding per subject: each owner binds a subject of its own.
			b.Spec.Subjects[0] = rolebinding.Subject{Kind: rolebinding.SubjectGroup, Name: "g-" + string(owner.Kind)}
		}
		if err := store.Upsert(ctx, b); err != nil {
			t.Fatalf("upsert %s-owned: %v", owner.Kind, err)
		}
		got, err := store.Get(ctx, b.Meta.ID)
		if err != nil || got == nil {
			t.Fatalf("get %s-owned: %v (row %v)", owner.Kind, err, got)
		}
		wantProject := ""
		if owner.Kind == meta.OwnerProject {
			wantProject = owner.ID
		}
		if got.Meta.Owner != owner || got.Spec.ProjectID != wantProject {
			t.Fatalf("%s-owned read back as owner %+v projectId %q", owner.Kind, got.Meta.Owner, got.Spec.ProjectID)
		}
	}
	if err := teams.Delete(ctx, tm.Meta.ID); err != nil {
		t.Fatalf("delete team: %v", err)
	}
	all, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, b := range all {
		if b.Meta.Owner.Kind == meta.OwnerTeam {
			t.Fatalf("binding %s outlived its team", b.Meta.Name)
		}
	}
}

// A subject with an id is held by one binding: a second binding naming it is refused by the store's own index, so two writes racing past the save-time check cannot both land. Group subjects may repeat.
func TestIntegration_SubjectHeldByOneBinding(t *testing.T) {
	ctx := context.Background()
	db := storagetest.Pool(t)
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "one-binding", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	if err := team.NewStore(db).Upsert(ctx, tm); err != nil {
		t.Fatalf("upsert team: %v", err)
	}
	prj := &project.Project{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "one-binding", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: tm.Meta.ID}},
		Spec: project.Spec{TeamID: tm.Meta.ID},
	}
	if err := project.NewStore(db).Upsert(ctx, prj); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	pol := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "one-binding", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	if err := policy.NewStore(db).Upsert(ctx, pol); err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	store := policybinding.NewStore(db)
	bind := func(name string, subjects ...rolebinding.Subject) error {
		return store.Upsert(ctx, &policybinding.PolicyBinding{
			Meta: meta.Metadata{ID: meta.NewID(), Name: name},
			Spec: policybinding.Spec{ProjectID: prj.Meta.ID, PolicyID: pol.Meta.ID, Subjects: subjects},
		})
	}
	group := rolebinding.Subject{Kind: rolebinding.SubjectGroup, Name: "system:authenticated"}
	project := rolebinding.Subject{Kind: policybinding.SubjectProject, ID: prj.Meta.ID}
	if err := bind("first", project, group); err != nil {
		t.Fatalf("first binding: %v", err)
	}
	if err := bind("second", project); !errors.Is(err, policybinding.ErrSubjectBound) {
		t.Fatalf("second binding of the project: err = %v, want ErrSubjectBound", err)
	}
	if err := bind("third", rolebinding.Subject{Kind: policybinding.SubjectTeam, ID: tm.Meta.ID}, group); err != nil {
		t.Fatalf("another subject beside a repeated group: %v", err)
	}
}

// A user is held by one binding per owner scope: a second project may bind the same user, the same project may not. Overlapping scopes (a team and its project) are refcheck's to refuse; the index sees only equal ones.
func TestIntegration_UserHeldByOneBindingPerScope(t *testing.T) {
	ctx := context.Background()
	db := storagetest.Pool(t)
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "per-scope", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	if err := team.NewStore(db).Upsert(ctx, tm); err != nil {
		t.Fatalf("upsert team: %v", err)
	}
	mkProject := func(name string) *project.Project {
		p := &project.Project{
			Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerTeam, ID: tm.Meta.ID}},
			Spec: project.Spec{TeamID: tm.Meta.ID},
		}
		if err := project.NewStore(db).Upsert(ctx, p); err != nil {
			t.Fatalf("upsert project: %v", err)
		}
		return p
	}
	first, second := mkProject("per-scope-1"), mkProject("per-scope-2")
	pol := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "per-scope", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	if err := policy.NewStore(db).Upsert(ctx, pol); err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	u := &user.User{ID: meta.NewID(), Username: "per-scope-alice"}
	if err := user.NewStore(db).Upsert(ctx, u); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	store := policybinding.NewStore(db)
	alice := rolebinding.Subject{Kind: rolebinding.SubjectUser, ID: u.ID}
	bind := func(name string, p *project.Project) error {
		return store.Upsert(ctx, &policybinding.PolicyBinding{
			Meta: meta.Metadata{ID: meta.NewID(), Name: name},
			Spec: policybinding.Spec{ProjectID: p.Meta.ID, PolicyID: pol.Meta.ID, Subjects: []rolebinding.Subject{alice}},
		})
	}
	if err := bind("alice-in-first", first); err != nil {
		t.Fatalf("first project binds alice: %v", err)
	}
	if err := bind("alice-in-second", second); err != nil {
		t.Fatalf("second project binds alice: %v", err)
	}
	if err := bind("alice-in-first-again", first); !errors.Is(err, policybinding.ErrSubjectBound) {
		t.Fatalf("first project binds alice twice: err = %v, want ErrSubjectBound", err)
	}
}

// A user-owned binding round-trips with its owner, holds the user once in the user's own scope beside a project's binding of the same user, and goes when the user is deleted.
func TestIntegration_UserOwnedBinding(t *testing.T) {
	ctx := context.Background()
	db := storagetest.Pool(t)
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "own-scope", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	if err := team.NewStore(db).Upsert(ctx, tm); err != nil {
		t.Fatalf("upsert team: %v", err)
	}
	prj := &project.Project{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "own-scope", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: tm.Meta.ID}},
		Spec: project.Spec{TeamID: tm.Meta.ID},
	}
	if err := project.NewStore(db).Upsert(ctx, prj); err != nil {
		t.Fatalf("upsert project: %v", err)
	}
	users := user.NewStore(db)
	u := &user.User{ID: meta.NewID(), Username: "own-scope-alice"}
	if err := users.Upsert(ctx, u); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	pol := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "own-scope", Owner: meta.Owner{Kind: meta.OwnerUser, ID: u.ID}}}
	if err := policy.NewStore(db).Upsert(ctx, pol); err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	store := policybinding.NewStore(db)
	alice := rolebinding.Subject{Kind: rolebinding.SubjectUser, ID: u.ID}
	own := func(name string) *policybinding.PolicyBinding {
		return &policybinding.PolicyBinding{
			Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerUser, ID: u.ID}},
			Spec: policybinding.Spec{PolicyID: pol.Meta.ID, Subjects: []rolebinding.Subject{alice}},
		}
	}
	mine := own("alice-own")
	if err := store.Upsert(ctx, mine); err != nil {
		t.Fatalf("upsert user-owned: %v", err)
	}
	got, err := store.Get(ctx, mine.Meta.ID)
	if err != nil || got == nil {
		t.Fatalf("get user-owned: %v (row %v)", err, got)
	}
	if got.Meta.Owner != mine.Meta.Owner || got.Spec.ProjectID != "" {
		t.Fatalf("user-owned read back as owner %+v projectId %q", got.Meta.Owner, got.Spec.ProjectID)
	}
	if err := store.Upsert(ctx, own("alice-own-again")); !errors.Is(err, policybinding.ErrSubjectBound) {
		t.Fatalf("second user-owned binding of alice: err = %v, want ErrSubjectBound", err)
	}
	// The index holds one per scope, so a project's binding of alice stands beside her own.
	if err := store.Upsert(ctx, &policybinding.PolicyBinding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "alice-in-project"},
		Spec: policybinding.Spec{ProjectID: prj.Meta.ID, PolicyID: pol.Meta.ID, Subjects: []rolebinding.Subject{alice}},
	}); err != nil {
		t.Fatalf("project binds alice beside her own binding: %v", err)
	}
	if err := users.Delete(ctx, u.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if got, err := store.Get(ctx, mine.Meta.ID); err != nil || got != nil {
		t.Fatalf("user-owned binding after its user was deleted = %v (err %v), want gone", got, err)
	}
}
