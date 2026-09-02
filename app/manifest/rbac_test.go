package manifest_test

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/rolebinding"
)

const rbacYAML = `
apiVersion: relay.wyolet.dev/v1alpha2
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
apiVersion: relay.wyolet.dev/v1alpha2
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
apiVersion: relay.wyolet.dev/v1alpha2
kind: RoleBinding
metadata:
  name: everyone-viewer
spec:
  role: developer
  scope: {kind: system}
  subjects:
    - {kind: group, name: system:authenticated}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: PolicyBinding
metadata:
  name: ml-search-everyone
spec:
  project: ml-search
  policy: ml-search-default
  subjects:
    - {kind: group, name: system:authenticated}
`

const roleUUID = "0195f8a0-0000-7000-8000-000000000022"

var rbacResolver = manifest.MapResolver{
	Teams:           map[string]string{"platform": teamUUID},
	Projects:        map[string]string{"ml-search": projectUUID},
	Policies:        map[string]string{"ml-search-default": policyUUID},
	ServiceAccounts: map[string]string{"search-indexer": saUUID},
	Roles:           map[string]string{"developer": roleUUID},
	Users:           map[string]string{"alice": aliceUUID},
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
	if pb.Spec.ProjectID != projectUUID || pb.Spec.PolicyID != policyUUID {
		t.Errorf("policy binding refs = %+v", pb.Spec)
	}
	if rendered := manifest.FromPolicyBinding(pb, rbacRev); rendered.Spec.Priority != 100 {
		t.Errorf("rendered priority = %d, want the default 100", rendered.Spec.Priority)
	}
}

// TestToPolicyBinding_OmittedPriorityStampsTheDefaultOnSpec pins the value
// on Spec.Priority itself, not just what EffectivePriority reports:
// re-applying the same document must produce the identical row the store
// already stamped, or apply would see a spurious diff every run.
func TestToPolicyBinding_OmittedPriorityStampsTheDefaultOnSpec(t *testing.T) {
	docs, err := manifest.Parse(strings.NewReader(rbacYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	pb, err := manifest.ToPolicyBinding(*docs[3].PolicyBinding, rbacResolver)
	if err != nil {
		t.Fatalf("ToPolicyBinding: %v", err)
	}
	if pb.Spec.Priority != policybinding.DefaultPriority {
		t.Errorf("Spec.Priority = %d, want the default %d stamped directly", pb.Spec.Priority, policybinding.DefaultPriority)
	}
}

func TestToRoleBinding_UnknownScopeTarget(t *testing.T) {
	const yaml = `
apiVersion: relay.wyolet.dev/v1alpha2
kind: RoleBinding
metadata:
  name: ghost-team
spec:
  role: developer
  scope: {kind: team, name: nowhere}
  subjects:
    - {kind: group, name: platform-eng}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: RoleBinding
metadata:
  name: ghost-project
spec:
  role: developer
  scope: {kind: project, name: nowhere}
  subjects:
    - {kind: group, name: platform-eng}
`
	docs, err := manifest.Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := manifest.ToRoleBinding(*docs[0].RoleBinding, rbacResolver); err == nil ||
		!strings.Contains(err.Error(), `team "nowhere" not found`) {
		t.Errorf("ToRoleBinding = %v, want an unknown-team error", err)
	}
	if _, err := manifest.ToRoleBinding(*docs[1].RoleBinding, rbacResolver); err == nil ||
		!strings.Contains(err.Error(), `project "nowhere" not found`) {
		t.Errorf("ToRoleBinding = %v, want an unknown-project error", err)
	}
}

func TestToRoleBinding_UnknownSubject(t *testing.T) {
	const yaml = `
apiVersion: relay.wyolet.dev/v1alpha2
kind: RoleBinding
metadata:
  name: ghosts
spec:
  role: developer
  scope: {kind: system}
  subjects:
    - {kind: serviceaccount, name: nobody}
`
	docs, err := manifest.Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := manifest.ToRoleBinding(*docs[0].RoleBinding, rbacResolver); err == nil ||
		!strings.Contains(err.Error(), "service account") {
		t.Fatalf("ToRoleBinding = %v, want an unknown-service-account error", err)
	}
}
