package manifest_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/manifest"
)

func TestPlanReportsTheChangedFields(t *testing.T) {
	e := newEnv(t)
	e.apply(t, bundle, manifest.Options{})
	edited := strings.NewReplacer(
		"displayName: Platform", "displayName: Platform Eng",
		`costCenter: "1042"`, `costCenter: "2001"`,
		"budget: q3", "budget: q4",
		"members: [alice, bob]", "members: [alice]",
		"email: alice@example.com", "email: alice@corp.example.com",
		"{kinds: [models], verbs: [get, list]}", "{kinds: [models, keys], verbs: [get, list]}",
	).Replace(bundle)
	p := e.plan(t, e.parse(t, edited), manifest.Options{})
	wantLines(t, actions(p), []string{
		"update Team/platform metadata.displayName,spec.costCenter",
		"unchanged Project/search",
		"update ServiceAccount/indexer spec.budget",
		"update Role/model-reader spec.rules",
		"update User/alice spec.email",
		"unchanged User/bob",
		"update Group/eng spec.members",
		"unchanged RoleBinding/search-readers",
	})
	if _, err := p.Apply(context.Background(), e.dir, admin); err != nil {
		t.Fatal(err)
	}
	for _, entry := range e.plan(t, e.parse(t, edited), manifest.Options{}).Entries {
		if entry.Action != manifest.ActionUnchanged {
			t.Errorf("after apply: %s %s/%s %v", entry.Action, entry.Kind, entry.Name, entry.ChangedFields)
		}
	}
}

// A row an operator edited by hand is left alone unless the run is forced.
func TestPlanSkipsDirtyRowsUnlessForced(t *testing.T) {
	e := newEnv(t)
	e.apply(t, bundle, manifest.Options{})
	ctx := context.Background()
	teams := e.store.Stores().Teams
	rows, _ := teams.List(ctx)
	rows[0].Meta.DisplayName, rows[0].Meta.Dirty = "Edited", true
	if err := teams.Upsert(ctx, rows[0]); err != nil {
		t.Fatal(err)
	}
	if got := e.plan(t, e.parse(t, bundle), manifest.Options{}).Entries[0]; got.Action != manifest.ActionSkipDirty || len(got.ChangedFields) == 0 {
		t.Errorf("team = %s %v, want skip-dirty with the differing fields", got.Action, got.ChangedFields)
	}
	e.apply(t, bundle, manifest.Options{Force: true})
	rows, _ = teams.List(ctx)
	if rows[0].Meta.DisplayName != "Platform" || rows[0].Meta.Dirty {
		t.Errorf("forced apply left %+v", rows[0].Meta)
	}
}

// A hand-edited row the documents already match, such as an exported edit applied back, is not drift.
func TestPlanReportsADirtyRowMatchingTheDocumentAsUnchanged(t *testing.T) {
	e := newEnv(t)
	e.apply(t, bundle, manifest.Options{})
	ctx := context.Background()
	teams := e.store.Stores().Teams
	rows, _ := teams.List(ctx)
	rows[0].Meta.Dirty = true
	if err := teams.Upsert(ctx, rows[0]); err != nil {
		t.Fatal(err)
	}
	if got := e.plan(t, e.parse(t, bundle), manifest.Options{}).Entries[0]; got.Action != manifest.ActionUnchanged {
		t.Errorf("team = %s %v, want unchanged", got.Action, got.ChangedFields)
	}
}

// Prune deletes the manifest-owned rows the documents dropped and nothing created outside them, whatever kind.
func TestPruneLeavesRowsCreatedOutsideManifestsAlone(t *testing.T) {
	e := newEnv(t)
	e.apply(t, bundle, manifest.Options{})
	ctx := context.Background()
	// A team, project and group created by hand (through the directory, as a UI would), without the selector label.
	handTeam := &directory.Team{Meta: directory.Metadata{Name: "ui-team"}}
	if err := e.dir.CreateTeam(ctx, handTeam); err != nil {
		t.Fatal(err)
	}
	if err := e.dir.CreateProject(ctx, &directory.Project{Meta: directory.Metadata{Name: "ui-project"}, Spec: directory.ProjectSpec{TeamID: handTeam.Meta.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := e.dir.CreateGroup(ctx, admin, &directory.Group{Meta: directory.Metadata{Name: "ui-group", Labels: map[string]string{"managed-by": "someone-else"}}}); err != nil {
		t.Fatal(err)
	}
	if err := e.dir.CreateUser(ctx, nil, &directory.User{Username: "carol"}); err != nil {
		t.Fatal(err)
	}
	// Keep only the team and the project; drop the account, role, group and binding documents.
	kept := strings.Join(strings.Split(bundle, "---\n")[:2], "---\n")
	if _, err := e.reg.Plan(ctx, e.store.Stores(), e.parse(t, kept), manifest.Options{Prune: true}); !errors.Is(err, manifest.ErrSelectorRequired) {
		t.Fatalf("prune without a selector = %v, want ErrSelectorRequired", err)
	}
	p := e.apply(t, kept, manifest.Options{Prune: true, Selector: map[string]string{"managed-by": "gitops"}})
	wantLines(t, actions(p), []string{
		"unchanged Team/platform", "unchanged Project/search",
		"delete RoleBinding/search-readers", "delete Group/eng", "delete Role/model-reader", "delete ServiceAccount/indexer",
	})
	s := e.store.Stores()
	teams, _ := s.Teams.List(ctx)
	projects, _ := s.Projects.List(ctx)
	groups, _ := s.Groups.List(ctx)
	users, _ := s.Users.List(ctx)
	if len(teams) != 2 || len(projects) != 2 || len(groups) != 1 || groups[0].Meta.Name != "ui-group" || len(users) != 3 {
		t.Errorf("after prune: %d teams, %d projects, groups %v, %d users", len(teams), len(projects), groups, len(users))
	}
}

func TestPlanRefusesADuplicateAndAnUnresolvedName(t *testing.T) {
	e := newEnv(t)
	dup := "apiVersion: auth.wyolet.dev/v1alpha1\nkind: Team\nmetadata: {name: a}\n---\napiVersion: auth.wyolet.dev/v1alpha1\nkind: Team\nmetadata: {name: a}\n"
	_, err := e.reg.Plan(context.Background(), e.store.Stores(), e.parse(t, dup), manifest.Options{})
	if d := asError(t, err); d.Doc != 1 || !strings.Contains(d.Error(), "declared twice") {
		t.Errorf("duplicate = %v", d)
	}
	orphan := "apiVersion: auth.wyolet.dev/v1alpha1\nkind: Project\nmetadata: {name: p}\nspec: {team: missing}\n"
	_, err = e.reg.Plan(context.Background(), e.store.Stores(), e.parse(t, orphan), manifest.Options{})
	var ref *manifest.RefError
	if o := asError(t, err); o.Field != "spec.team" || !errors.As(err, &ref) || ref.Name != "missing" {
		t.Errorf("unresolved team = %v", err)
	}
	if !strings.Contains(err.Error(), `doc 0 Project "p": spec.team: team "missing" not found`) {
		t.Errorf("message = %q", err.Error())
	}
}

// The writes go through the directory, so its grant check decides a binding.
func TestApplyRunsTheDirectoryGrantCheck(t *testing.T) {
	e := newEnv(t)
	cfg := testConfig()
	denying := &denyBindings{}
	dir, err := directory.New(directory.Config{Stores: e.store.Stores(), Grants: denying, Kinds: cfg.RuleKinds, Verbs: cfg.RuleVerbs})
	if err != nil {
		t.Fatal(err)
	}
	p := e.plan(t, e.parse(t, bundle), manifest.Options{})
	applied, err := p.Apply(context.Background(), dir, admin)
	var ae *manifest.ApplyError
	if !errors.As(err, &ae) || ae.Entry.Kind != manifest.KindRoleBinding || len(applied) != 7 || !denying.called {
		t.Fatalf("apply = %v (%d applied), want the binding refused by the grant check", err, len(applied))
	}
}

// A member of a bound group acts under its bindings, so a document adding one runs the grant check; one removing a member does not.
func TestApplyChecksGroupMembersAsGrants(t *testing.T) {
	e := newEnv(t)
	e.apply(t, bundle, manifest.Options{})
	cfg := testConfig()
	denying := &denyBindings{}
	dir, err := directory.New(directory.Config{Stores: e.store.Stores(), Grants: denying, Kinds: cfg.RuleKinds, Verbs: cfg.RuleVerbs})
	if err != nil {
		t.Fatal(err)
	}
	fewer := strings.Replace(bundle, "members: [alice, bob]", "members: [alice]", 1)
	if _, err := e.plan(t, e.parse(t, fewer), manifest.Options{}).Apply(context.Background(), dir, admin); err != nil || denying.called {
		t.Fatalf("removing a member = %v (checked %v), want no grant check", err, denying.called)
	}
	_, err = e.plan(t, e.parse(t, bundle), manifest.Options{}).Apply(context.Background(), dir, admin)
	var ae *manifest.ApplyError
	if !errors.As(err, &ae) || ae.Entry.Kind != manifest.KindGroup || !denying.called {
		t.Fatalf("adding a member = %v, want the group refused by the grant check", err)
	}
}
