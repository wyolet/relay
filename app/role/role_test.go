package role

import (
	"slices"
	"testing"

	"github.com/wyolet/relay/pkg/meta"
)

func fix(rules ...Rule) *Role {
	return &Role{
		Meta: meta.Metadata{ID: "01a19bc3-0000-7000-8000-000000000001", Name: "custom", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: Spec{Rules: rules},
	}
}

// inSet binary-searches the vocabularies, so an entry added out of order would validate as unknown.
func TestVocabularyIsSortedAndUnique(t *testing.T) {
	for name, set := range map[string][]string{"Kinds": Kinds, "Verbs": Verbs} {
		sorted := slices.Clone(set)
		slices.Sort(sorted)
		if !slices.Equal(set, sorted) || len(slices.Compact(sorted)) != len(set) {
			t.Errorf("%s is not sorted and unique: %v", name, set)
		}
		for _, v := range set {
			if !inSet(set, v) {
				t.Errorf("%s: %q is listed but not recognised", name, v)
			}
		}
		for _, v := range []string{"", "!", "zzz", set[1] + "x"} {
			if inSet(set, v) {
				t.Errorf("%s: %q is recognised but not listed", name, v)
			}
		}
	}
}

func TestIsEnabled(t *testing.T) {
	off, on := false, true
	for _, tc := range []struct {
		enabled *bool
		want    bool
	}{{nil, true}, {&on, true}, {&off, false}} {
		if got := (&Role{Spec: Spec{Enabled: tc.enabled}}).IsEnabled(); got != tc.want {
			t.Errorf("IsEnabled with %v = %v, want %v", tc.enabled, got, tc.want)
		}
	}
}

func TestValidate(t *testing.T) {
	keysGet := Rule{Kinds: []string{"keys"}, Verbs: []string{"get"}}
	ownedBy := func(o meta.Owner) *Role {
		r := fix(keysGet)
		r.Meta.Owner = o
		return r
	}
	for _, tc := range []struct {
		name string
		role *Role
		want string
	}{
		{"known kinds and verbs", fix(Rule{Kinds: []string{"keys", "policies"}, Verbs: []string{"get", "list"}}), ""},
		{"wildcards", fix(Rule{Kinds: []string{"*"}, Verbs: []string{"*"}}), ""},
		{"system owner", ownedBy(meta.Owner{Kind: meta.OwnerSystem}), ""},
		{"organization owner", ownedBy(meta.Global), ""},
		{"unknown kind", fix(keysGet, Rule{Kinds: []string{"keys", "widgets"}, Verbs: []string{"get"}}), "Key: 'Role.Spec.Rules[1].Kinds[1]' Error:Field validation for 'Kinds[1]' failed on the 'rbackind' tag"},
		{"singular kind", fix(Rule{Kinds: []string{"key"}, Verbs: []string{"get"}}), "Key: 'Role.Spec.Rules[0].Kinds[0]' Error:Field validation for 'Kinds[0]' failed on the 'rbackind' tag"},
		{"unknown verb", fix(Rule{Kinds: []string{"keys"}, Verbs: []string{"frobnicate"}}), "Key: 'Role.Spec.Rules[0].Verbs[0]' Error:Field validation for 'Verbs[0]' failed on the 'rbacverb' tag"},
		{"no rules", fix(), "Key: 'Role.Spec.Rules' Error:Field validation for 'Rules' failed on the 'required' tag"},
		{"a rule without kinds", fix(Rule{Verbs: []string{"get"}}), "Key: 'Role.Spec.Rules[0].Kinds' Error:Field validation for 'Kinds' failed on the 'required' tag"},
		{"a rule without verbs", fix(Rule{Kinds: []string{"keys"}, Verbs: []string{}}), "Key: 'Role.Spec.Rules[0].Verbs' Error:Field validation for 'Verbs' failed on the 'min' tag"},
		{"project owner", ownedBy(meta.Owner{Kind: meta.OwnerProject, ID: "01a19bc3-0000-7000-8000-000000000002"}), `role "custom": owner.kind must be system, organization or user, got "project"`},
		{"no owner", ownedBy(meta.Owner{}), `role "custom": owner.kind must be system, organization or user, got ""`},
	} {
		got := ""
		if err := tc.role.Validate(); err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("%s:\n got %q\nwant %s", tc.name, got, tc.want)
		}
	}
}

func TestAllows(t *testing.T) {
	developer := fix(
		Rule{Kinds: []string{"policies", "keys"}, Verbs: []string{"get", "list"}},
		Rule{Kinds: []string{"keys"}, Verbs: []string{"rotate"}},
	)
	admin := fix(Rule{Kinds: []string{"*"}, Verbs: []string{"*"}})
	reader := fix(Rule{Kinds: []string{"*"}, Verbs: []string{"read"}})
	for _, tc := range []struct {
		name       string
		role       *Role
		kind, verb string
		want       bool
	}{
		{"listed kind and verb", developer, "keys", "list", true},
		{"verb from the second rule", developer, "keys", "rotate", true},
		{"verb not granted on that kind", developer, "policies", "rotate", false},
		{"kind not listed", developer, "teams", "get", false},
		{"wildcard kind and verb", admin, "teams", "delete", true},
		{"wildcard kind, named verb", reader, "usage", "read", true},
		{"wildcard kind, other verb", reader, "usage", "delete", false},
		{"no rules", fix(), "keys", "get", false},
	} {
		if got := tc.role.Allows(tc.kind, tc.verb); got != tc.want {
			t.Errorf("%s: Allows(%q, %q) = %v, want %v", tc.name, tc.kind, tc.verb, got, tc.want)
		}
	}
}
