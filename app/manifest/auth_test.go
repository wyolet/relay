package manifest_test

import (
	"github.com/google/go-cmp/cmp"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/meta"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/manifest"
	authmanifest "github.com/wyolet/relay/auth/manifest"
)

// authKindsYAML holds every kind the auth module owns that relay has a DTO for.
const authKindsYAML = `apiVersion: relay.wyolet.dev/v1alpha3
kind: Team
metadata: {name: platform, owner: {kind: user}, labels: {env: prod}}
spec: {enabled: true}
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Project
metadata: {name: ml-search, annotations: {note: x}}
spec: {team: platform}
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: ServiceAccount
metadata: {name: search-indexer}
spec: {project: ml-search, enabled: false}
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Group
metadata: {name: eng}
spec: {members: [alice]}
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Role
metadata: {name: developer}
spec: {rules: [{kinds: [keys], verbs: [get, list]}]}
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: RoleBinding
metadata: {name: devs}
spec: {role: developer, scope: {kind: team, name: platform}, subjects: [{kind: group, name: eng}, {kind: user, name: alice}]}
`

// The same documents translate to the same rows under either apiVersion.
func TestAuthKindsParseTheSameUnderEitherAPIVersion(t *testing.T) {
	relay := parseDocs(t, authKindsYAML, 6)
	// The auth apiVersion refuses metadata.owner; relay's ignores it, so dropping it changes no row.
	moved := strings.ReplaceAll(authKindsYAML, manifest.APIVersion, authmanifest.APIVersion)
	moved = strings.Replace(moved, " owner: {kind: user},", "", 1)
	auth := parseDocs(t, moved, 6)
	idx := manifest.MapResolver{
		Teams: map[string]string{"platform": teamUUID}, Projects: map[string]string{"ml-search": projectUUID},
		Users: map[string]string{"alice": aliceUUID}, Roles: map[string]string{"developer": roleUUID},
	}
	on, off := true, false
	system, inTeam := meta.Owner{Kind: meta.OwnerSystem}, meta.Owner{Kind: meta.OwnerTeam, ID: teamUUID}
	want := []any{
		&team.Team{Meta: meta.Metadata{Name: "platform", Owner: system, Labels: map[string]string{"env": "prod"}}, Spec: team.Spec{Enabled: &on}},
		&project.Project{Meta: meta.Metadata{Name: "ml-search", Owner: inTeam, Annotations: map[string]string{"note": "x"}}, Spec: project.Spec{TeamID: teamUUID}},
		&serviceaccount.ServiceAccount{Meta: meta.Metadata{Name: "search-indexer", Owner: meta.Owner{Kind: meta.OwnerProject, ID: projectUUID}}, Spec: serviceaccount.Spec{ProjectID: projectUUID, Enabled: &off}},
		&group.Group{Meta: meta.Metadata{Name: "eng", Owner: system}, Spec: group.Spec{MemberIDs: []string{aliceUUID}}},
		&role.Role{Meta: meta.Metadata{Name: "developer", Owner: system}, Spec: role.Spec{Rules: []role.Rule{{Kinds: []string{"keys"}, Verbs: []string{"get", "list"}}}}},
		&rolebinding.RoleBinding{Meta: meta.Metadata{Name: "devs", Owner: inTeam}, Spec: rolebinding.Spec{RoleID: roleUUID, Scope: inTeam, Subjects: []rolebinding.Subject{
			{Kind: rolebinding.SubjectGroup, Name: "eng"}, {Kind: rolebinding.SubjectUser, ID: aliceUUID},
		}}},
	}
	for name, docs := range map[string][]manifest.Document{"relay": relay, "auth": auth} {
		got, kinds := make([]any, len(docs)), ""
		for i, d := range docs {
			got[i], kinds = translate(t, d, idx), kinds+" "+d.Kind()
		}
		if diff := cmp.Diff(want, got); diff != "" || kinds != " Team Project ServiceAccount Group Role RoleBinding" {
			t.Errorf("under the %s apiVersion: kinds%s, rows (-want +got):\n%s", name, kinds, diff)
		}
	}
}

func translate(t *testing.T, d manifest.Document, idx manifest.MapResolver) any {
	t.Helper()
	row, err := relayTranslate(d, idx)
	if err != nil {
		t.Fatalf("%s: %v", d.Kind(), err)
	}
	return row
}

// strictCases fail under the auth apiVersion and parse under relay's, which keeps its leniency.
var strictCases = []struct{ name, doc, want string }{
	{"unknown field", "kind: Group\nmetadata: {name: g}\nspec: {membrs: [a]}", `wire: doc 1 Group "g": spec.membrs: unknown field "membrs"`},
	{"rule typo", "kind: Role\nmetadata: {name: r}\nspec: {rules: [{kinds: [models], verbs: [updat]}]}", `wire: doc 1 Role "r": spec.rules[0].verbs[0]: "updat" is not a registered verb`},
	{"owner", "kind: Team\nmetadata: {name: t, owner: {kind: user}}", "wire: doc 1 Team: metadata.owner: metadata.owner is not accepted: the owner is derived from the kind and its spec (the system for Team, Role and Group, the parent for Project and ServiceAccount, the scope for RoleBinding)"},
}

// Under the auth apiVersion the module decodes these kinds strictly: each problem fails at parse, naming the document and the field.
func TestAuthKindsParseStrictlyUnderTheAuthAPIVersion(t *testing.T) {
	for _, tc := range strictCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := manifest.Parse(strings.NewReader("apiVersion: auth.wyolet.dev/v1alpha1\nkind: Team\nmetadata: {name: first}\n---\napiVersion: auth.wyolet.dev/v1alpha1\n" + tc.doc + "\n"))
			if got := errText(err); got != tc.want {
				t.Errorf("%s:\n got %q\nwant %s", tc.name, got, tc.want)
			}
		})
	}
}

// Under relay's apiVersion the same documents parse as they always have: stray fields and metadata.owner are ignored, and a rule typo is left to the row's Validate at plan time.
func TestAuthKindsStayLenientUnderRelayAPIVersion(t *testing.T) {
	for _, tc := range strictCases {
		t.Run(tc.name, func(t *testing.T) {
			parseOne(t, "apiVersion: relay.wyolet.dev/v1alpha3\n"+tc.doc+"\n")
		})
	}
}

// A User under the auth apiVersion is an account document; under any other it stays the identity loader's and LoadDir walks past it.
func TestUserDocumentsByAPIVersion(t *testing.T) {
	docs, err := manifest.Parse(strings.NewReader(`apiVersion: auth.wyolet.dev/v1alpha1
kind: User
metadata: {name: alice}
spec: {email: alice@example.com, password: {valueFrom: {env: ALICE_PW}}}
---
apiVersion: relay.wyolet.dev/v1
kind: User
metadata: {name: admin}
spec: {username: admin, password: plain-is-fine-here}
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 || docs[0].User == nil || docs[0].Kind() != "User" || docs[0].User.Metadata.Name != "alice" || docs[1].Foreign != "User" {
		t.Fatalf("docs = %+v", docs)
	}
	_, err = manifest.Parse(strings.NewReader("apiVersion: auth.wyolet.dev/v1alpha1\nkind: Policy\nmetadata: {name: p}\n"))
	if got, want := errText(err), `wire: doc 0: unsupported apiVersion "auth.wyolet.dev/v1alpha1" (want "relay.wyolet.dev/v1alpha3")`; got != want {
		t.Errorf("a relay kind under the auth apiVersion:\n got %q\nwant %s", got, want)
	}
	// The CLI re-renders what it parsed before sending it; an account must survive that.
	out, err := manifest.Render(docs[:1], "")
	if err != nil {
		t.Fatal(err)
	}
	again, err := manifest.Parse(strings.NewReader(string(out)))
	if err != nil || len(again) != 1 || again[0].User == nil || again[0].User.User.Password.ValueFrom.Env != "ALICE_PW" {
		t.Fatalf("re-rendered account = %+v, %v:\n%s", again, err, out)
	}
}

// Rendering a row through the module keeps relay's wire form: relay's apiVersion and the owner block naming the parent.
func TestAuthKindsRenderInRelayForm(t *testing.T) {
	docs := parseDocs(t, authKindsYAML, 6)
	idx := manifest.MapResolver{Teams: map[string]string{"platform": teamUUID}, Projects: map[string]string{"ml-search": projectUUID}}
	rev := manifest.MapReverseResolver{Teams: map[string]string{teamUUID: "platform"}, Projects: map[string]string{projectUUID: "ml-search"}}

	sa := translate(t, docs[2], idx).(*serviceaccount.ServiceAccount)
	wantSA := *docs[2].ServiceAccount
	wantSA.Metadata.Owner = manifest.WireOwner{Kind: meta.OwnerProject, Name: "ml-search"}
	exported(t, "FromServiceAccount", wantSA, manifest.FromServiceAccount(sa, rev))

	tm := translate(t, docs[0], idx).(*team.Team)
	// The document's own owner is ignored: a team is the system's.
	wantTeam := *docs[0].Team
	wantTeam.Metadata.Owner = manifest.WireOwner{Kind: meta.OwnerSystem}
	exported(t, "FromTeam", wantTeam, manifest.FromTeam(tm, rev))

	p := translate(t, docs[1], idx).(*project.Project)
	wantProject := *docs[1].Project
	wantProject.Metadata.Owner = manifest.WireOwner{Kind: meta.OwnerTeam, Name: "platform"}
	exported(t, "FromProject", wantProject, manifest.FromProject(p, rev))
}
