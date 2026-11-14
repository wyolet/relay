package manifest

import (
	"strings"
	"testing"

	authmanifest "github.com/wyolet/relay/auth/manifest"
	"github.com/wyolet/relay/pkg/meta"
)

// A manifest may not choose who owns a Team, Group or Role: those rows name a scope or a grant, and a user-owned one would hand its author every binding already made against that name.
func TestTenancyKindsAreAlwaysSystemOwned(t *testing.T) {
	idx := MapResolver{Users: map[string]string{"mallory": "u-mallory"}}
	md := WireMeta{Name: "x", Owner: WireOwner{Kind: meta.OwnerUser, Name: "mallory"}}
	owners := map[string]meta.Owner{}
	tm, err := ToTeam(TeamDTO{Metadata: md}, idx)
	if err != nil {
		t.Fatalf("ToTeam: %v", err)
	}
	owners["team"] = tm.Meta.Owner
	g, err := ToGroup(GroupDTO{Metadata: md}, idx)
	if err != nil {
		t.Fatalf("ToGroup: %v", err)
	}
	owners["group"] = g.Meta.Owner
	r, err := ToRole(RoleDTO{Metadata: md, Spec: RoleSpec{Rules: []RoleRuleDTO{{Kinds: []string{"usage"}, Verbs: []string{"read"}}}}}, idx)
	if err != nil {
		t.Fatalf("ToRole: %v", err)
	}
	owners["role"] = r.Meta.Owner
	for kind, o := range owners {
		if o != (meta.Owner{Kind: meta.OwnerSystem}) {
			t.Errorf("%s owner = %+v, want system", kind, o)
		}
	}
}

// A user or host owner is named on the wire like every other cross-reference; leaving the name in place would make the stored owner unresolvable.
func TestOwnerNameResolvesToID(t *testing.T) {
	idx := MapResolver{Users: map[string]string{"alice": "u-alice"}, Hosts: map[string]string{"acme-api": "h-acme"}}
	k, err := ToKey(KeyDTO{
		Metadata: WireMeta{Name: "alice-cli", Owner: WireOwner{Kind: meta.OwnerUser, Name: "alice"}},
		Spec:     KeySpec{Principal: PrincipalDTO{Kind: "user", Name: "alice"}},
	}, idx)
	if want := (meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}); err != nil || k.Meta.Owner != want || k.Spec.Principal.ID != "u-alice" {
		t.Errorf("ToKey owner %+v principal %+v, %v; want both resolved to u-alice", k.Meta.Owner, k.Spec.Principal, err)
	}
	hk, err := ToHostKey(HostKeyDTO{
		Metadata: WireMeta{Name: "acme-key", Owner: WireOwner{Kind: meta.OwnerHost, Name: "acme-api"}},
		Spec:     HostKeySpec{HostID: "acme-api"},
	}, idx)
	if want := (meta.Owner{Kind: meta.OwnerHost, ID: "h-acme"}); err != nil || hk.Meta.Owner != want || hk.Spec.HostID != "h-acme" {
		t.Errorf("ToHostKey owner %+v host %q, %v; want both resolved to h-acme", hk.Meta.Owner, hk.Spec.HostID, err)
	}
}

// A field v1alpha3 dropped from one of relay's kinds is refused by name with what replaces it, under either apiVersion and the auth module's: ignoring it would drop a restriction the author meant.
func TestRemovedFieldsAreRefused(t *testing.T) {
	for _, tc := range []struct{ name, doc, want string }{
		{"a key's policy", "apiVersion: " + APIVersion + "\nkind: Key\nmetadata: {name: ci-key}\nspec: {principal: {kind: serviceaccount, name: ci}, keyHash: x, policy: basic}\n", "wire: doc 0 kind=Key name=ci-key: Key no longer has spec.policy (use a PolicyBinding on the account the key runs in)"},
		{"a team's budget", "apiVersion: " + APIVersion + "\nkind: Team\nmetadata: {name: platform}\nspec: {budget: {amount: \"100\"}}\n", "wire: doc 0 kind=Team name=platform: Team no longer has spec.budget (use a Budget linked by the policy bound to the team)"},
		{"a project's budget, previous apiVersion", "apiVersion: " + PreviousAPIVersion + "\nkind: Project\nmetadata: {name: search}\nspec: {team: platform, budget: {amount: \"100\", period: week}}\n", "wire: doc 0 kind=Project name=search: Project no longer has spec.budget (use a Budget linked by the policy bound to the project)"},
		{"a team's budget, the auth module's apiVersion", "apiVersion: " + authmanifest.APIVersion + "\nkind: Team\nmetadata: {name: platform}\nspec: {budget: {amount: \"100\"}}\n", `wire: doc 0 Team "platform": spec.budget: unknown field "budget"`},
	} {
		_, err := Parse(strings.NewReader(tc.doc))
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("%s:\n got %q\nwant %s", tc.name, got, tc.want)
		}
	}
}
