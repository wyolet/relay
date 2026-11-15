package manifest_test

import (
	"errors"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/role"
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

// Names resolve to ids, a binding's owner mirrors its scope, and each row exports back to the document it came from.
func TestRoundTrip_RBAC(t *testing.T) {
	docs := parseDocs(t, rbacYAML, 4)
	rev := rbacRev
	rev.Accounts = map[string]string{projectAccountUUID: "project-ml-search"}
	org := meta.Global
	team := meta.Owner{Kind: meta.OwnerTeam, ID: teamUUID}

	// The document's system owner is not the manifest's to choose: a role is the organization's.
	r := translated(t, "ToRole", &role.Role{
		Meta: meta.Metadata{Name: "developer", Owner: org},
		Spec: role.Spec{Rules: []role.Rule{
			{Kinds: []string{"keys", "service-accounts"}, Verbs: []string{"get", "list", "create", "rotate"}},
			{Kinds: []string{"usage", "logs"}, Verbs: []string{"read"}},
		}},
	})(manifest.ToRole(*docs[0].Role, rbacResolver))
	wantRole := *docs[0].Role
	wantRole.Metadata.Owner = manifest.WireOwner{Kind: meta.OwnerOrganization}
	exported(t, "FromRole", wantRole, manifest.FromRole(r, rev))

	scoped := translated(t, "ToRoleBinding", &rolebinding.RoleBinding{
		Meta: meta.Metadata{Name: "platform-admins", Owner: team},
		Spec: rolebinding.Spec{RoleID: roleUUID, Scope: team, Subjects: []rolebinding.Subject{
			{Kind: rolebinding.SubjectGroup, Name: "platform-eng"},
			{Kind: rolebinding.SubjectUser, ID: aliceUUID},
			{Kind: rolebinding.SubjectServiceAccount, ID: saUUID},
		}},
	})(manifest.ToRoleBinding(*docs[1].RoleBinding, rbacResolver))
	wantDoc := *docs[1].RoleBinding
	wantDoc.Metadata.Owner = manifest.WireOwner{Kind: meta.OwnerTeam, Name: "platform"}
	exported(t, "FromRoleBinding", wantDoc, manifest.FromRoleBinding(scoped, rev))

	// A system scope is the global scope's earlier name: it reads, and renders, as the organization.
	global := translated(t, "ToRoleBinding system-scoped", &rolebinding.RoleBinding{
		Meta: meta.Metadata{Name: "everyone-viewer", Owner: org},
		Spec: rolebinding.Spec{RoleID: roleUUID, Scope: org, Subjects: []rolebinding.Subject{{Kind: rolebinding.SubjectGroup, Name: "system:authenticated"}}},
	})(manifest.ToRoleBinding(*docs[2].RoleBinding, rbacResolver))
	wantDoc = *docs[2].RoleBinding
	wantDoc.Metadata.Owner = manifest.WireOwner{Kind: meta.OwnerOrganization}
	wantDoc.Spec.Scope = manifest.WireOwner{Kind: meta.OwnerOrganization}
	exported(t, "FromRoleBinding system-scoped", wantDoc, manifest.FromRoleBinding(global, rev))

	pb := translated(t, "ToPolicyBinding", &policybinding.PolicyBinding{
		Meta: meta.Metadata{Name: "ml-search-everyone", Owner: meta.Owner{Kind: meta.OwnerProject, ID: projectUUID}},
		Spec: policybinding.Spec{PolicyID: policyUUID, AccountIDs: []string{projectAccountUUID}},
	})(manifest.ToPolicyBinding(*docs[3].PolicyBinding, rbacResolver))
	exported(t, "FromPolicyBinding", *docs[3].PolicyBinding, manifest.FromPolicyBinding(pb, rev))

	// What apply would write must pass each row's own rules once it has an id.
	r.Meta.ID, scoped.Meta.ID, global.Meta.ID, pb.Meta.ID = meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID()
	for kind, err := range map[string]error{"role": r.Validate(), "team binding": scoped.Validate(), "system binding": global.Validate(), "policy binding": pb.Validate()} {
		if err != nil {
			t.Errorf("%s does not validate: %v", kind, err)
		}
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
