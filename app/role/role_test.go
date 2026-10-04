package role

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
)

func fix(rules ...Rule) *Role {
	return &Role{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "custom", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: Spec{Rules: rules},
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		role *Role
		want string // "" = valid
	}{
		{
			name: "known kinds and verbs",
			role: fix(Rule{Kinds: []string{"keys", "policies"}, Verbs: []string{"get", "list"}}),
		},
		{
			name: "wildcards accepted",
			role: fix(Rule{Kinds: []string{"*"}, Verbs: []string{"*"}}),
		},
		{
			name: "unknown kind",
			role: fix(Rule{Kinds: []string{"widgets"}, Verbs: []string{"get"}}),
			want: "rbackind",
		},
		{
			name: "unknown verb",
			role: fix(Rule{Kinds: []string{"keys"}, Verbs: []string{"frobnicate"}}),
			want: "rbacverb",
		},
		{
			name: "no rules",
			role: fix(),
			want: "Rules",
		},
		{
			name: "system owner accepted",
			role: func() *Role {
				r := fix(Rule{Kinds: []string{"*"}, Verbs: []string{"*"}})
				r.Meta.Owner = meta.Owner{Kind: meta.OwnerSystem}
				return r
			}(),
		},
		{
			name: "project owner rejected",
			role: func() *Role {
				r := fix(Rule{Kinds: []string{"keys"}, Verbs: []string{"get"}})
				r.Meta.Owner = meta.Owner{Kind: meta.OwnerProject, ID: meta.NewID()}
				return r
			}(),
			want: "owner.kind must be system or user",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.role.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case tc.want != "" && err == nil:
				t.Fatalf("Validate() = nil, want error mentioning %q", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("Validate() = %v, want error mentioning %q", err, tc.want)
			}
		})
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
		name string
		role *Role
		kind string
		verb string
		want bool
	}{
		{name: "listed kind and verb", role: developer, kind: "keys", verb: "list", want: true},
		{name: "verb from the second rule", role: developer, kind: "keys", verb: "rotate", want: true},
		{name: "verb not granted on that kind", role: developer, kind: "policies", verb: "rotate"},
		{name: "kind not listed", role: developer, kind: "teams", verb: "get"},
		{name: "wildcard kind and verb", role: admin, kind: "teams", verb: "delete", want: true},
		{name: "wildcard kind, named verb", role: reader, kind: "usage", verb: "read", want: true},
		{name: "wildcard kind, other verb", role: reader, kind: "usage", verb: "delete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.role.Allows(tc.kind, tc.verb); got != tc.want {
				t.Fatalf("Allows(%q, %q) = %v, want %v", tc.kind, tc.verb, got, tc.want)
			}
		})
	}
}

func TestBuiltins(t *testing.T) {
	roles, err := Builtins()
	if err != nil {
		t.Fatalf("Builtins: %v", err)
	}
	want := []string{"admin", "auditor", "catalog-editor", "team-admin", "project-admin", "developer", "viewer"}
	if len(roles) != len(want) {
		t.Fatalf("got %d built-in roles, want %d", len(roles), len(want))
	}
	for i, name := range want {
		if roles[i].Meta.Name != name {
			t.Errorf("built-in %d = %q, want %q", i, roles[i].Meta.Name, name)
		}
		roles[i].Meta.ID = meta.NewID()
		if err := roles[i].Validate(); err != nil {
			t.Errorf("built-in %q: %v", name, err)
		}
		if roles[i].Meta.Owner.Kind != meta.OwnerSystem {
			t.Errorf("built-in %q owner = %q, want system", name, roles[i].Meta.Owner.Kind)
		}
		if !IsBuiltin(name) {
			t.Errorf("IsBuiltin(%q) = false", name)
		}
	}
	if IsBuiltin("custom") {
		t.Error("IsBuiltin(\"custom\") = true")
	}
	if !roles[0].Allows("teams", "delete") {
		t.Error("admin should allow every verb on every kind")
	}
}
