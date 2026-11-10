// binding_filter_test.go covers the two RBAC binding list filters — the
// lookups that answer "who has access here". Same table shape as the other
// filter tests in list_schemas_test.go; its names() helper is a closed type
// switch that does not carry the binding kinds, so these read Meta.Name.

package control

import (
	"net/url"
	"slices"
	"testing"

	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/rolebinding"
)

func TestRoleBindingFilter(t *testing.T) {
	off := false
	user := func(id string) rolebinding.Subject {
		return rolebinding.Subject{Kind: rolebinding.SubjectUser, ID: id}
	}
	grp := func(name string) rolebinding.Subject {
		return rolebinding.Subject{Kind: rolebinding.SubjectGroup, Name: name}
	}
	items := []*rolebinding.RoleBinding{
		{Meta: meta.Metadata{Name: "alice-team"}, Spec: rolebinding.Spec{RoleID: "r-admin",
			Scope: meta.Owner{Kind: meta.OwnerTeam, ID: "t1"}, Subjects: []rolebinding.Subject{user("u1")}}},
		{Meta: meta.Metadata{Name: "bob-project"}, Spec: rolebinding.Spec{RoleID: "r-dev",
			Scope: meta.Owner{Kind: meta.OwnerProject, ID: "p1"}, Subjects: []rolebinding.Subject{user("u2"), grp("ml")}}},
		{Meta: meta.Metadata{Name: "ml-global"}, Spec: rolebinding.Spec{RoleID: "r-dev", Enabled: &off,
			Scope: meta.Owner{Kind: meta.OwnerSystem}, Subjects: []rolebinding.Subject{grp("ml")}}},
	}

	for _, tc := range []struct {
		q    string
		want []string
	}{
		{"subject=user%3Au2", []string{"bob-project"}},
		{"subject=group%3Aml", []string{"bob-project", "ml-global"}},
		{"scope_kind=team", []string{"alice-team"}},
		{"scope_kind=project&scope_id=p1", []string{"bob-project"}},
		{"scope_id=t1", []string{"alice-team"}},
		{"role_id=r-dev", []string{"bob-project", "ml-global"}},
		{"role_id=r-dev&enabled=true", []string{"bob-project"}},
	} {
		got, _ := applyQ(t, roleBindingFilter, tc.q, items)
		names := make([]string, len(got))
		for i, b := range got {
			names[i] = b.Meta.Name
		}
		if !slices.Equal(names, tc.want) {
			t.Errorf("%s => %v, want %v", tc.q, names, tc.want)
		}
	}
	if _, err := roleBindingFilter.Parse(url.Values{"scope_kind": {"user"}}); err == nil {
		t.Error("scope_kind=user is outside the enum and should 400")
	}
	if _, err := roleBindingFilter.Parse(url.Values{"bogus": {"1"}}); err == nil {
		t.Error("unknown key should 400")
	}
}

func TestPolicyBindingFilter(t *testing.T) {
	p1, p2 := meta.Owner{Kind: meta.OwnerProject, ID: "p1"}, meta.Owner{Kind: meta.OwnerProject, ID: "p2"}
	items := []*policybinding.PolicyBinding{
		{Meta: meta.Metadata{Name: "search-default", Owner: p1}, Spec: policybinding.Spec{PolicyID: "pol1", AccountIDs: []string{"a-project"}}},
		{Meta: meta.Metadata{Name: "indexer-override", Owner: p1}, Spec: policybinding.Spec{PolicyID: "pol2", AccountIDs: []string{"a-sa1"}}},
		{Meta: meta.Metadata{Name: "lab-default", Owner: p2}, Spec: policybinding.Spec{PolicyID: "pol1", AccountIDs: []string{"a-lab"}}},
		{Meta: meta.Metadata{Name: "tiers", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: policybinding.Spec{PolicyID: "pol3", AccountIDs: []string{"a-org", "a-sa1-x"}}},
	}

	for _, tc := range []struct {
		q    string
		want []string
	}{
		// DefaultSort is name, so every expectation is in name order.
		{"owner_id=p1", []string{"indexer-override", "search-default"}},
		{"owner=system", []string{"tiers"}},
		{"policy_id=pol1", []string{"lab-default", "search-default"}},
		{"owner_id=p1&policy_id=pol2", []string{"indexer-override"}},
		{"account_id=a-sa1", []string{"indexer-override"}},
		{"account_id=a-org", []string{"tiers"}},
	} {
		got, _ := applyQ(t, policyBindingFilter, tc.q, items)
		names := make([]string, len(got))
		for i, b := range got {
			names[i] = b.Meta.Name
		}
		if !slices.Equal(names, tc.want) {
			t.Errorf("%s => %v, want %v", tc.q, names, tc.want)
		}
	}
	if _, err := policyBindingFilter.Parse(url.Values{"scope_kind": {"team"}}); err == nil {
		t.Error("policy bindings have no scope_kind and should 400")
	}
}
