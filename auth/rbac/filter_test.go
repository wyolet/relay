package rbac

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wyolet/relay/auth/authz"
)

// ownRows is an owner shortcut that also answers ScopeOf.
type ownRows struct{}

func (ownRows) Decide(_ context.Context, p *Principal, _, _ string, res authz.Resource) (Decision, error) {
	if res.Owner != nil && res.Owner.Kind == "person" && res.Owner.ID == p.ID {
		return Allow, nil
	}
	return Continue, nil
}

func (ownRows) Filter(_ context.Context, p *Principal, _, _ string) (Filter, error) {
	return Filter{Owners: []authz.Owner{{Kind: "person", ID: p.ID}}}, nil
}

// fixedFilter answers ScopeOf with a constant.
type fixedFilter struct {
	f   Filter
	err error
}

func (fixedFilter) Decide(context.Context, *Principal, string, string, authz.Resource) (Decision, error) {
	return Continue, nil
}

func (r fixedFilter) Filter(context.Context, *Principal, string, string) (Filter, error) {
	return r.f, r.err
}

func TestScopeOf(t *testing.T) {
	ctx := context.Background()
	bindings := map[string][]Binding{
		"user:r":  {{RoleID: "reader", Scope: folder1}, {RoleID: "reader", Scope: folder1}, {RoleID: "missing", Scope: global}},
		"group:g": {{RoleID: "reader", Scope: folder2}, {RoleID: "editor", Scope: org1}},
		"user:a":  {{RoleID: "anyget", Scope: global}},
	}
	readDocs := []Rule{{Kinds: []string{"documents"}, Verbs: []string{"get"}}}

	tests := []struct {
		name  string
		p     *Principal
		rules []ProductRule
		verb  string
		want  Filter
	}{
		{"admin", &Principal{Admin: true}, nil, "get", Filter{All: true}},
		{"global binding", user("u", "user:a"), []ProductRule{ownRows{}}, "get", Filter{All: true}},
		{"scoped bindings, deduplicated", user("u", "user:r", "group:g"), nil, "get",
			Filter{Scopes: []Scope{folder1, folder2, org1}}},
		{"role must cover the verb", user("u", "user:r", "group:g"), nil, "delete", Filter{Scopes: []Scope{org1}}},
		{"rule filters join the bindings", user("u", "user:r"), []ProductRule{ownRows{}, fixedFilter{f: Filter{OwnerKinds: []string{"shared"}, Scopes: []Scope{folder1}}}}, "get",
			Filter{Scopes: []Scope{folder1}, Owners: []authz.Owner{{Kind: "person", ID: "u"}}, OwnerKinds: []string{"shared"}}},
		{"a rule admitting everything", user("u"), []ProductRule{fixedFilter{f: Filter{All: true, Scopes: []Scope{folder1}}}}, "get", Filter{All: true}},
		{"nothing", user("u"), nil, "get", Filter{}},
		{"capability narrows to nothing", &Principal{ID: "u", Subjects: []string{"user:r"}, Credential: &CredentialScope{Rules: readDocs}}, nil, "list", Filter{}},
		{"capability keeps", &Principal{ID: "u", Subjects: []string{"user:r"}, Credential: &CredentialScope{Rules: readDocs}}, nil, "get",
			Filter{Scopes: []Scope{folder1}}},
		{"id list", &Principal{ID: "u", Subjects: []string{"user:r"}, Credential: &CredentialScope{Rules: readDocs, IDs: map[string][]string{"documents": {"d1"}}}}, nil, "get",
			Filter{Scopes: []Scope{folder1}, IDs: []string{"d1"}}},
		{"empty id list", &Principal{ID: "u", Subjects: []string{"user:r"}, Credential: &CredentialScope{Rules: readDocs, IDs: map[string][]string{"documents": {}}}}, nil, "get",
			Filter{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, _ := newEngine(bindings, tt.rules...)
			got, err := e.ScopeOf(ctx, tt.p, tt.verb, "documents")
			if err != nil {
				t.Fatalf("ScopeOf: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ScopeOf = %+v, want %+v", got, tt.want)
			}
			if got.None() != (!tt.want.All && tt.want.Scopes == nil && tt.want.Owners == nil && tt.want.OwnerKinds == nil) {
				t.Fatalf("None() = %v for %+v", got.None(), got)
			}
		})
	}
}

func TestScopeOfFailsClosed(t *testing.T) {
	ctx := context.Background()
	p := user("u", "user:r")
	plain := RuleFunc(func(context.Context, *Principal, string, string, authz.Resource) (Decision, error) {
		return Continue, nil
	})
	bound := map[string][]Binding{"user:r": {{RoleID: "reader", Scope: folder1}}}

	for _, tt := range []struct {
		name  string
		p     *Principal
		rules []ProductRule
		setup func(*Engine, *memSource)
		want  error
	}{
		{"unauthenticated", nil, nil, nil, authz.ErrUnauthenticated},
		{"a rule without a filter", p, []ProductRule{plain}, nil, ErrUnfilterable},
		{"a rule filter with its own id limit", p, []ProductRule{fixedFilter{f: Filter{IDs: []string{"x"}}}}, nil, ErrUnfilterable},
		{"a rule filter error", p, []ProductRule{fixedFilter{err: errDown}}, nil, errDown},
		{"binding lookup", p, nil, func(_ *Engine, s *memSource) { s.bindingErr = errDown }, errDown},
		{"role lookup", p, nil, func(_ *Engine, s *memSource) { s.roleErr = errDown }, errDown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e, src := newEngine(bound, tt.rules...)
			if tt.setup != nil {
				tt.setup(e, src)
			}
			f, err := e.ScopeOf(ctx, tt.p, "get", "documents")
			if !errors.Is(err, tt.want) || !f.None() {
				t.Fatalf("ScopeOf = %+v, %v; want nothing, %v", f, err, tt.want)
			}
		})
	}
}

func TestScopeOfWithoutSource(t *testing.T) {
	e := &Engine{Rules: []ProductRule{ownRows{}}}
	f, err := e.ScopeOf(context.Background(), user("u"), "get", "documents")
	if err != nil || !reflect.DeepEqual(f, Filter{Owners: []authz.Owner{{Kind: "person", ID: "u"}}}) {
		t.Fatalf("ScopeOf = %+v, %v", f, err)
	}
}
