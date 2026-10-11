package manifest_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/manifest"
)

// bundle declares one row of every kind, each referring to the others by name.
const bundle = `apiVersion: auth.wyolet.dev/v1alpha1
kind: Team
metadata: {name: platform, displayName: Platform, labels: {managed-by: gitops}}
spec: {costCenter: "1042"}
---
apiVersion: auth.wyolet.dev/v1alpha1
kind: Project
metadata: {name: search, labels: {managed-by: gitops}}
spec: {team: platform}
---
apiVersion: auth.wyolet.dev/v1alpha1
kind: ServiceAccount
metadata: {name: indexer, labels: {managed-by: gitops}}
spec: {project: search, budget: q3}
---
apiVersion: auth.wyolet.dev/v1alpha1
kind: Role
metadata: {name: model-reader, labels: {managed-by: gitops}}
spec:
  rules:
    - {kinds: [models], verbs: [get, list]}
---
apiVersion: auth.wyolet.dev/v1alpha1
kind: User
metadata: {name: alice}
spec:
  email: alice@example.com
  password: {valueFrom: {env: ALICE_PASSWORD}}
  identity: {issuer: "https://idp.example.com", subject: alice}
  roles: [model-reader]
---
apiVersion: auth.wyolet.dev/v1alpha1
kind: User
metadata: {name: bob}
spec:
  password: {hash: "$2a$10$CwTycUXWue0Thq9StjUM0uJ8.6Y3bOS2jvUJFXJ3m/BS/YwgMm9aW"}
---
apiVersion: auth.wyolet.dev/v1alpha1
kind: Group
metadata: {name: eng, labels: {managed-by: gitops}}
spec: {members: [alice, bob]}
---
apiVersion: auth.wyolet.dev/v1alpha1
kind: RoleBinding
metadata: {name: search-readers, labels: {managed-by: gitops}}
spec:
  role: model-reader
  scope: {kind: project, name: search}
  subjects: [{kind: group, name: eng}, {kind: user, name: bob}, {kind: serviceaccount, name: indexer}]
`

func TestApplyCreatesEveryKindInDependencyOrder(t *testing.T) {
	e := newEnv(t)
	p := e.apply(t, bundle, manifest.Options{})
	wantLines(t, actions(p), []string{
		"create Team/platform", "create Project/search", "create ServiceAccount/indexer", "create Role/model-reader",
		"create User/alice", "create User/bob", "create Group/eng", "create RoleBinding/search-readers",
	})
	ctx := context.Background()
	s := e.store.Stores()
	teams, _ := s.Teams.List(ctx)
	if string(teams[0].Spec.Attributes["costCenter"]) != `"1042"` || teams[0].Meta.Owner.Kind != directory.OwnerSystem {
		t.Errorf("team = %+v", teams[0])
	}
	sas, _ := s.ServiceAccounts.List(ctx)
	if string(sas[0].Spec.Attributes["budgetId"]) != `"budget-q3"` {
		t.Errorf("service account attributes = %s", sas[0].Spec.Attributes)
	}
	alice, _ := s.Users.ByUsername(ctx, "alice")
	if alice == nil || alice.PasswordHash == testEnv["ALICE_PASSWORD"] || alice.Identity.Subject != "alice" {
		t.Fatalf("alice = %+v", alice)
	}
	groups, _ := s.Groups.List(ctx)
	if len(groups[0].Spec.MemberIDs) != 2 || groups[0].Spec.MemberIDs[0] != alice.ID {
		t.Errorf("group members = %v", groups[0].Spec.MemberIDs)
	}
	bindings, _ := s.RoleBindings.List(ctx)
	if b := bindings[0]; b.Spec.Scope.Kind != directory.OwnerProject || len(b.Spec.Subjects) != 3 || b.Meta.Owner.Kind != directory.OwnerProject {
		t.Errorf("binding = %+v", b)
	}
}

// Rendering what Apply stored and parsing it again gives documents that plan as unchanged, for every kind.
func TestEveryKindRoundTripsThroughRender(t *testing.T) {
	e := newEnv(t)
	e.apply(t, bundle, manifest.Options{})
	rendered := renderAll(t, e)
	again := e.plan(t, e.parse(t, string(rendered)), manifest.Options{})
	for _, entry := range again.Entries {
		if entry.Action != manifest.ActionUnchanged {
			t.Errorf("%s %s/%s %v after a round trip, want unchanged\n%s", entry.Action, entry.Kind, entry.Name, entry.ChangedFields, rendered)
		}
	}
	if len(again.Entries) != 8 {
		t.Errorf("round trip planned %d entries, want 8", len(again.Entries))
	}
	// The original documents still plan as unchanged too: the env password verifies against the stored hash.
	for _, entry := range e.plan(t, e.parse(t, bundle), manifest.Options{}).Entries {
		if entry.Action != manifest.ActionUnchanged {
			t.Errorf("re-plan of the bundle: %s %s/%s %v", entry.Action, entry.Kind, entry.Name, entry.ChangedFields)
		}
	}
}

// renderAll renders every stored row through the From functions.
func renderAll(t *testing.T, e *env) []byte {
	t.Helper()
	ctx := context.Background()
	s := e.store.Stores()
	names := storeNames(t, e)
	var docs []*manifest.Document
	add := func(d *manifest.Document, err error) {
		if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, d)
	}
	teams, _ := s.Teams.List(ctx)
	for _, x := range teams {
		add(e.reg.FromTeam(x, names))
	}
	projects, _ := s.Projects.List(ctx)
	for _, x := range projects {
		add(e.reg.FromProject(x, names))
	}
	sas, _ := s.ServiceAccounts.List(ctx)
	for _, x := range sas {
		add(e.reg.FromServiceAccount(x, names))
	}
	roles, _ := s.Roles.List(ctx)
	for _, x := range roles {
		add(e.reg.FromRole(x, names))
	}
	users, _ := s.Users.List(ctx)
	for _, x := range users {
		add(e.reg.FromUser(x), nil)
	}
	groups, _ := s.Groups.List(ctx)
	for _, x := range groups {
		add(e.reg.FromGroup(x, names))
	}
	bindings, _ := s.RoleBindings.List(ctx)
	for _, x := range bindings {
		add(e.reg.FromRoleBinding(x, names))
	}
	out, err := manifest.Render(docs)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte("budget: q3")) || !bytes.Contains(out, []byte("team: platform")) {
		t.Errorf("rendered names missing:\n%s", out)
	}
	return out
}

// namer is an id→name lookup over every stored row and the product kinds.
type namer map[string]map[string]string

func (n namer) Name(kind, id string) (string, bool) {
	v, ok := n[kind][id]
	return v, ok
}

func storeNames(t *testing.T, e *env) namer {
	t.Helper()
	ctx := context.Background()
	s := e.store.Stores()
	n := namer{"Budget": {"budget-q3": "q3", "budget-q4": "q4"}}
	put := func(kind, id, name string) {
		if n[kind] == nil {
			n[kind] = map[string]string{}
		}
		n[kind][id] = name
	}
	teams, _ := s.Teams.List(ctx)
	for _, x := range teams {
		put(manifest.KindTeam, x.Meta.ID, x.Meta.Name)
	}
	projects, _ := s.Projects.List(ctx)
	for _, x := range projects {
		put(manifest.KindProject, x.Meta.ID, x.Meta.Name)
	}
	sas, _ := s.ServiceAccounts.List(ctx)
	for _, x := range sas {
		put(manifest.KindServiceAccount, x.Meta.ID, x.Meta.Name)
	}
	roles, _ := s.Roles.List(ctx)
	for _, x := range roles {
		put(manifest.KindRole, x.Meta.ID, x.Meta.Name)
	}
	users, _ := s.Users.List(ctx)
	for _, x := range users {
		put(manifest.KindUser, x.ID, x.Username)
	}
	return n
}
