package manifest_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/manifest"
	authmanifest "github.com/wyolet/relay/auth/manifest"
)

// authKindsYAML holds every kind the auth module owns that relay has a DTO for, relay's own fields (budget, policy) included.
const authKindsYAML = `apiVersion: relay.wyolet.dev/v1alpha2
kind: Team
metadata: {name: platform, owner: {kind: user}, labels: {env: prod}}
spec: {budget: {amount: "100"}}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Project
metadata: {name: ml-search, annotations: {note: x}}
spec: {team: platform, budget: {amount: "5", period: week}}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: ServiceAccount
metadata: {name: search-indexer}
spec: {project: ml-search, policy: ml-search-default, enabled: false}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Group
metadata: {name: eng}
spec: {members: [alice]}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Role
metadata: {name: developer}
spec: {rules: [{kinds: [keys], verbs: [get, list]}]}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: RoleBinding
metadata: {name: devs}
spec: {role: developer, scope: {kind: team, name: platform}, subjects: [{kind: group, name: eng}, {kind: user, name: alice}]}
`

// The same documents parse to the same DTOs and translate to the same rows under either apiVersion.
func TestAuthKindsParseTheSameUnderEitherAPIVersion(t *testing.T) {
	relay, err := manifest.Parse(strings.NewReader(authKindsYAML))
	if err != nil {
		t.Fatalf("relay apiVersion: %v", err)
	}
	// The auth apiVersion refuses metadata.owner; relay's ignores it, so dropping it changes no row.
	moved := strings.ReplaceAll(authKindsYAML, manifest.APIVersion, authmanifest.APIVersion)
	moved = strings.Replace(moved, " owner: {kind: user},", "", 1)
	auth, err := manifest.Parse(strings.NewReader(moved))
	if err != nil {
		t.Fatalf("auth apiVersion: %v", err)
	}
	if len(relay) != 6 || len(auth) != 6 {
		t.Fatalf("parsed %d and %d documents", len(relay), len(auth))
	}
	idx := manifest.MapResolver{
		Teams: map[string]string{"platform": teamUUID}, Projects: map[string]string{"ml-search": projectUUID},
		Policies: map[string]string{"ml-search-default": policyUUID}, Users: map[string]string{"alice": aliceUUID},
		Roles: map[string]string{"developer": roleUUID},
	}
	for i := range relay {
		a, b := relay[i], auth[i]
		if a.Kind() != b.Kind() {
			t.Fatalf("doc %d: %s vs %s", i, a.Kind(), b.Kind())
		}
		rowA, rowB := translate(t, a, idx), translate(t, b, idx)
		if !reflect.DeepEqual(rowA, rowB) {
			t.Errorf("%s rows differ:\n relay %+v\n auth  %+v", a.Kind(), rowA, rowB)
		}
	}
	if relay[0].Team.Spec.Budget == nil || relay[0].Team.Spec.Budget.Amount != "100" || relay[2].ServiceAccount.Spec.Policy != "ml-search-default" {
		t.Errorf("relay fields lost: %+v %+v", relay[0].Team.Spec, relay[2].ServiceAccount.Spec)
	}
}

func translate(t *testing.T, d manifest.Document, idx manifest.Resolver) any {
	t.Helper()
	var row any
	var err error
	switch {
	case d.Team != nil:
		row, err = manifest.ToTeam(*d.Team, idx)
	case d.Project != nil:
		row, err = manifest.ToProject(*d.Project, idx)
	case d.ServiceAccount != nil:
		row, err = manifest.ToServiceAccount(*d.ServiceAccount, idx)
	case d.Group != nil:
		row, err = manifest.ToGroup(*d.Group, idx)
	case d.Role != nil:
		row, err = manifest.ToRole(*d.Role, idx)
	case d.RoleBinding != nil:
		row, err = manifest.ToRoleBinding(*d.RoleBinding, idx)
	}
	if err != nil {
		t.Fatalf("%s: %v", d.Kind(), err)
	}
	return row
}

// strictCases fail under the auth apiVersion and parse under relay's, which keeps its leniency.
var strictCases = []struct{ name, doc, want string }{
	{"unknown field", "kind: Group\nmetadata: {name: g}\nspec: {membrs: [a]}", `spec.membrs: unknown field "membrs"`},
	{"rule typo", "kind: Role\nmetadata: {name: r}\nspec: {rules: [{kinds: [models], verbs: [updat]}]}", `spec.rules[0].verbs[0]: "updat" is not a registered verb`},
	{"budget typo", "kind: Team\nmetadata: {name: t}\nspec: {budget: {amount: \"1\", amout: \"1\"}}", `spec.budget: json: unknown field "amout"`},
	{"owner", "kind: Team\nmetadata: {name: t, owner: {kind: user}}", `metadata.owner is not accepted`},
}

// Under the auth apiVersion the module decodes these kinds strictly: each problem fails at parse, naming the document and the field.
func TestAuthKindsParseStrictlyUnderTheAuthAPIVersion(t *testing.T) {
	for _, tc := range strictCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := manifest.Parse(strings.NewReader("apiVersion: auth.wyolet.dev/v1alpha1\nkind: Team\nmetadata: {name: first}\n---\napiVersion: auth.wyolet.dev/v1alpha1\n" + tc.doc + "\n"))
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "doc 1") {
				t.Fatalf("err = %v, want %q at doc 1", err, tc.want)
			}
		})
	}
}

// Under relay's apiVersion the same documents parse as they always have: stray fields and metadata.owner are ignored, and a rule typo is left to the row's Validate at plan time.
func TestAuthKindsStayLenientUnderRelayAPIVersion(t *testing.T) {
	for _, tc := range strictCases {
		t.Run(tc.name, func(t *testing.T) {
			docs, err := manifest.Parse(strings.NewReader("apiVersion: relay.wyolet.dev/v1alpha2\n" + tc.doc + "\n"))
			if err != nil || len(docs) != 1 {
				t.Fatalf("docs = %v, err = %v", docs, err)
			}
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
	if _, err := manifest.Parse(strings.NewReader("apiVersion: auth.wyolet.dev/v1alpha1\nkind: Policy\nmetadata: {name: p}\n")); err == nil {
		t.Error("a relay kind under the auth apiVersion parsed")
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
	docs, err := manifest.Parse(strings.NewReader(authKindsYAML))
	if err != nil {
		t.Fatal(err)
	}
	idx := manifest.MapResolver{
		Teams: map[string]string{"platform": teamUUID}, Projects: map[string]string{"ml-search": projectUUID},
		Policies: map[string]string{"ml-search-default": policyUUID},
	}
	rev := manifest.MapReverseResolver{
		Teams: map[string]string{teamUUID: "platform"}, Projects: map[string]string{projectUUID: "ml-search"},
		Policies: map[string]string{policyUUID: "ml-search-default"},
	}
	sa, err := manifest.ToServiceAccount(*docs[2].ServiceAccount, idx)
	if err != nil {
		t.Fatal(err)
	}
	back := manifest.FromServiceAccount(sa, rev)
	if back.APIVersion != manifest.APIVersion || back.Spec.Policy != "ml-search-default" || back.Spec.Project != "ml-search" ||
		back.Metadata.Owner.Kind != "project" || back.Metadata.Owner.Name != "ml-search" || back.Spec.Enabled == nil || *back.Spec.Enabled {
		t.Errorf("FromServiceAccount = %+v", back)
	}
	tm, err := manifest.ToTeam(*docs[0].Team, idx)
	if err != nil {
		t.Fatal(err)
	}
	if got := manifest.FromTeam(tm, rev); got.Spec.Budget == nil || got.Spec.Budget.Period != "month" || got.Metadata.Labels["env"] != "prod" {
		t.Errorf("FromTeam = %+v", got)
	}
}
