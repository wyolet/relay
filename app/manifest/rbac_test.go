package manifest_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/pkg/meta"
)

const rbacYAML = `
apiVersion: relay.wyolet.dev/v1alpha3
kind: Role
metadata:
  name: developer
  owner: {kind: system}
spec:
  rules:
    - kinds: [keys, service-accounts]
      verbs: [get, list, create, rotate]
    - kinds: [usage, logs]
      verbs: [read]
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: RoleBinding
metadata:
  name: platform-admins
spec:
  role: developer
  scope: {kind: team, name: platform}
  subjects:
    - {kind: group, name: platform-eng}
    - {kind: user, name: alice}
    - {kind: serviceaccount, name: search-indexer}
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: RoleBinding
metadata:
  name: everyone-viewer
spec:
  role: developer
  scope: {kind: system}
  subjects:
    - {kind: group, name: system:authenticated}
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: PolicyBinding
metadata:
  name: ml-search-everyone
  owner: {kind: project, name: ml-search}
spec:
  policy: ml-search-default
  accounts: [project-ml-search]
`

const (
	roleUUID           = "0195f8a0-0000-7000-8000-000000000022"
	projectAccountUUID = "0195f8a0-0000-7000-8000-000000000023"
)

var rbacResolver = manifest.MapResolver{
	Teams:           map[string]string{"platform": teamUUID},
	Projects:        map[string]string{"ml-search": projectUUID},
	Policies:        map[string]string{"ml-search-default": policyUUID},
	ServiceAccounts: map[string]string{"search-indexer": saUUID},
	Roles:           map[string]string{"developer": roleUUID},
	Users:           map[string]string{"alice": aliceUUID},
	Accounts:        map[string]string{"project-ml-search": projectAccountUUID},
}

var rbacRev = manifest.MapReverseResolver{
	Teams:           map[string]string{teamUUID: "platform"},
	Projects:        map[string]string{projectUUID: "ml-search"},
	Policies:        map[string]string{policyUUID: "ml-search-default"},
	ServiceAccounts: map[string]string{saUUID: "search-indexer"},
	Roles:           map[string]string{roleUUID: "developer"},
	Users:           map[string]string{aliceUUID: "alice"},
}

func TestRoundTrip_RBAC(t *testing.T) {
	docs, err := manifest.Parse(strings.NewReader(rbacYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(docs) != 4 || docs[0].Role == nil || docs[1].RoleBinding == nil ||
		docs[2].RoleBinding == nil || docs[3].PolicyBinding == nil {
		t.Fatalf("want Role + 2 RoleBinding + PolicyBinding docs, got %d", len(docs))
	}

	r, err := manifest.ToRole(*docs[0].Role, rbacResolver)
	if err != nil {
		t.Fatalf("ToRole: %v", err)
	}
	if err := func() error { r.Meta.ID = meta.NewID(); return r.Validate() }(); err != nil {
		t.Fatalf("role validate: %v", err)
	}
	if len(r.Spec.Rules) != 2 || r.Spec.Rules[0].Kinds[0] != "keys" {
		t.Fatalf("rules = %+v", r.Spec.Rules)
	}
	if back := manifest.FromRole(r, rbacRev); back.Spec.Rules[1].Verbs[0] != "read" {
		t.Errorf("FromRole rules = %+v", back.Spec.Rules)
	}

	scoped, err := manifest.ToRoleBinding(*docs[1].RoleBinding, rbacResolver)
	if err != nil {
		t.Fatalf("ToRoleBinding: %v", err)
	}
	scoped.Meta.ID = meta.NewID()
	if err := scoped.Validate(); err != nil {
		t.Fatalf("binding validate: %v", err)
	}
	if scoped.Spec.RoleID != roleUUID || scoped.Spec.Scope.ID != teamUUID {
		t.Errorf("scope/role = %+v", scoped.Spec)
	}
	if scoped.Meta.Owner != scoped.Spec.Scope {
		t.Errorf("owner %+v does not mirror scope %+v", scoped.Meta.Owner, scoped.Spec.Scope)
	}
	wantSubjects := []rolebinding.Subject{
		{Kind: rolebinding.SubjectGroup, Name: "platform-eng"},
		{Kind: rolebinding.SubjectUser, ID: aliceUUID},
		{Kind: rolebinding.SubjectServiceAccount, ID: saUUID},
	}
	for i, want := range wantSubjects {
		if scoped.Spec.Subjects[i] != want {
			t.Errorf("subject %d = %+v, want %+v", i, scoped.Spec.Subjects[i], want)
		}
	}
	back := manifest.FromRoleBinding(scoped, rbacRev)
	if back.Spec.Role != "developer" || back.Spec.Scope.Kind != meta.OwnerTeam || back.Spec.Scope.Name != "platform" {
		t.Errorf("FromRoleBinding = %+v", back.Spec)
	}
	if back.Spec.Subjects[0].Name != "platform-eng" || back.Spec.Subjects[1].Name != "alice" ||
		back.Spec.Subjects[2].Name != "search-indexer" {
		t.Errorf("FromRoleBinding subjects = %+v", back.Spec.Subjects)
	}

	global, err := manifest.ToRoleBinding(*docs[2].RoleBinding, rbacResolver)
	if err != nil {
		t.Fatalf("ToRoleBinding system-scoped: %v", err)
	}
	if global.Spec.Scope.Kind != meta.OwnerSystem || global.Spec.Scope.ID != "" {
		t.Errorf("system scope = %+v, want the system owner with no id", global.Spec.Scope)
	}
	rendered := manifest.FromRoleBinding(global, rbacRev)
	if rendered.Spec.Scope.Kind != meta.OwnerSystem || rendered.Spec.Scope.Name != "" {
		t.Errorf("system scope rendered as %+v, want {kind: system}", rendered.Spec.Scope)
	}

	pb, err := manifest.ToPolicyBinding(*docs[3].PolicyBinding, rbacResolver)
	if err != nil {
		t.Fatalf("ToPolicyBinding: %v", err)
	}
	pb.Meta.ID = meta.NewID()
	if err := pb.Validate(); err != nil {
		t.Fatalf("policy binding validate: %v", err)
	}
	if pb.Meta.Owner.ID != projectUUID || pb.Spec.PolicyID != policyUUID || len(pb.Spec.AccountIDs) != 1 || pb.Spec.AccountIDs[0] != projectAccountUUID {
		t.Errorf("policy binding refs = %+v owned by %+v", pb.Spec, pb.Meta.Owner)
	}
}

// A binding naming a role, scope or subject the resolver does not know is refused by name, as a row the caller may not see.
func TestToRoleBindingUnknownRefs(t *testing.T) {
	group := manifest.SubjectDTO{Kind: "group", Name: "platform-eng"}
	for _, tc := range []struct {
		name string
		spec manifest.RoleBindingSpec
		want string
	}{
		{"team", manifest.RoleBindingSpec{Role: "developer", Scope: manifest.WireOwner{Kind: meta.OwnerTeam, Name: "nowhere"}, Subjects: []manifest.SubjectDTO{group}}, `rolebinding "ghost": team "nowhere" not found`},
		{"project", manifest.RoleBindingSpec{Role: "developer", Scope: manifest.WireOwner{Kind: meta.OwnerProject, Name: "nowhere"}, Subjects: []manifest.SubjectDTO{group}}, `rolebinding "ghost": project "nowhere" not found`},
		{"role", manifest.RoleBindingSpec{Role: "nobody", Scope: manifest.WireOwner{Kind: meta.OwnerSystem}, Subjects: []manifest.SubjectDTO{group}}, `rolebinding "ghost": role "nobody" not found`},
		{"service account", manifest.RoleBindingSpec{Role: "developer", Scope: manifest.WireOwner{Kind: meta.OwnerSystem}, Subjects: []manifest.SubjectDTO{group, {Kind: "serviceaccount", Name: "nobody"}}}, `binding "ghost": service account "nobody" not found`},
		{"user", manifest.RoleBindingSpec{Role: "developer", Scope: manifest.WireOwner{Kind: meta.OwnerSystem}, Subjects: []manifest.SubjectDTO{{Kind: "user", Name: "nobody"}}}, `binding "ghost": user "nobody" not found`},
	} {
		_, err := manifest.ToRoleBinding(manifest.RoleBindingDTO{Metadata: manifest.WireMeta{Name: "ghost"}, Spec: tc.spec}, rbacResolver)
		var notFound *manifest.RefNotFoundError
		if got := errText(err); got != tc.want || !errors.As(err, &notFound) {
			t.Errorf("unknown %s:\n got %q\nwant %s", tc.name, got, tc.want)
		}
	}
}
