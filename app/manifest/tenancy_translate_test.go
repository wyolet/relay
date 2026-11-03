package manifest

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
	authmanifest "github.com/wyolet/relay/auth/manifest"
)

// A manifest may not choose who owns a Team, Group or Role: those rows name a
// scope or a grant, and a user-owned one would hand its author every binding
// already made against that name.
func TestTenancyKindsAreAlwaysSystemOwned(t *testing.T) {
	idx := MapResolver{Users: map[string]string{"mallory": "u-mallory"}}

	teamDoc := TeamDTO{Metadata: WireMeta{Name: "platform"}}
	teamDoc.Metadata.Owner = WireOwner{Kind: meta.OwnerUser, Name: "mallory"}
	tm, err := ToTeam(teamDoc, idx)
	if err != nil {
		t.Fatalf("ToTeam: %v", err)
	}
	if tm.Meta.Owner.Kind != meta.OwnerSystem || tm.Meta.Owner.ID != "" {
		t.Errorf("team owner = %+v, want system", tm.Meta.Owner)
	}

	groupDoc := GroupDTO{Metadata: WireMeta{Name: "platform-eng"}}
	groupDoc.Metadata.Owner = WireOwner{Kind: meta.OwnerUser, Name: "mallory"}
	g, err := ToGroup(groupDoc, idx)
	if err != nil {
		t.Fatalf("ToGroup: %v", err)
	}
	if g.Meta.Owner.Kind != meta.OwnerSystem || g.Meta.Owner.ID != "" {
		t.Errorf("group owner = %+v, want system", g.Meta.Owner)
	}

	roleDoc := RoleDTO{Metadata: WireMeta{Name: "auditor-lite"}}
	roleDoc.Metadata.Owner = WireOwner{Kind: meta.OwnerUser, Name: "mallory"}
	roleDoc.Spec.Rules = []RoleRuleDTO{{Kinds: []string{"usage"}, Verbs: []string{"read"}}}
	r, err := ToRole(roleDoc, idx)
	if err != nil {
		t.Fatalf("ToRole: %v", err)
	}
	if r.Meta.Owner.Kind != meta.OwnerSystem || r.Meta.Owner.ID != "" {
		t.Errorf("role owner = %+v, want system", r.Meta.Owner)
	}
}

// A user-owned row names its owner by username on the wire like every other
// cross-reference; leaving the username in place would make the stored owner
// unresolvable.
func TestUserOwnerNameResolvesToID(t *testing.T) {
	idx := MapResolver{Users: map[string]string{"alice": "u-alice"}}
	d := KeyDTO{Metadata: WireMeta{Name: "alice-cli"}}
	d.Metadata.Owner = WireOwner{Kind: meta.OwnerUser, Name: "alice"}
	d.Spec.Principal = PrincipalDTO{Kind: "user", Name: "alice"}

	k, err := ToKey(d, idx)
	if err != nil {
		t.Fatalf("ToKey: %v", err)
	}
	if k.Meta.Owner.ID != "u-alice" {
		t.Errorf("owner id = %q, want the resolved user id", k.Meta.Owner.ID)
	}
}

// Export names a host owner by slug, so a host-owned HostKey has to resolve
// it back to the id the way a host-owned Policy does.
func TestHostOwnerNameResolvesToID(t *testing.T) {
	idx := MapResolver{Hosts: map[string]string{"acme-api": "h-acme"}}
	d := HostKeyDTO{Metadata: WireMeta{Name: "acme-key"}}
	d.Metadata.Owner = WireOwner{Kind: meta.OwnerHost, Name: "acme-api"}
	d.Spec.HostID = "acme-api"

	k, err := ToHostKey(d, idx)
	if err != nil {
		t.Fatalf("ToHostKey: %v", err)
	}
	if k.Meta.Owner.ID != "h-acme" {
		t.Errorf("owner id = %q, want the resolved host id", k.Meta.Owner.ID)
	}
}

// A Key holds no policy: a v1alpha3 document still carrying spec.policy is
// refused rather than silently dropping the restriction it meant.
func TestKeyPolicyIsRefused(t *testing.T) {
	doc := "apiVersion: " + APIVersion + "\nkind: Key\nmetadata: {name: ci-key}\n" +
		"spec: {principal: {kind: serviceaccount, name: ci}, keyHash: x, policy: basic}\n"
	_, err := Parse(strings.NewReader(doc))
	if err == nil || !strings.Contains(err.Error(), "spec.policy") {
		t.Fatalf("Parse = %v, want a refusal naming spec.policy", err)
	}
}

// A Team or Project spend cap is a Budget kind now; a document still carrying spec.budget is refused rather than silently losing the cap it meant.
func TestScopeBudgetIsRefused(t *testing.T) {
	for _, doc := range []string{
		"apiVersion: " + APIVersion + "\nkind: Team\nmetadata: {name: platform}\nspec: {budget: {amount: \"100\"}}\n",
		"apiVersion: relay.wyolet.dev/v1alpha2\nkind: Project\nmetadata: {name: search}\nspec: {team: platform, budget: {amount: \"100\", period: week}}\n",
		"apiVersion: " + authmanifest.APIVersion + "\nkind: Team\nmetadata: {name: platform}\nspec: {budget: {amount: \"100\"}}\n",
	} {
		if _, err := Parse(strings.NewReader(doc)); err == nil || !strings.Contains(err.Error(), "budget") {
			t.Errorf("Parse(%q) err = %v, want a refusal naming spec.budget", doc, err)
		}
	}
}
