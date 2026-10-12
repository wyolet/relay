package rbac

import (
	"context"
	"testing"

	"github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/principal"
)

// staticSource is a Source with no bookkeeping, so a benchmark measures the engine alone.
type staticSource map[string][]Binding

func (s staticSource) BindingsForSubject(_ context.Context, subject string) ([]Binding, error) {
	return s[subject], nil
}

func (staticSource) Role(_ context.Context, id string) (Role, bool, error) {
	r, ok := roles[id]
	return r, ok, nil
}

// fixedChain answers every resource with the same chain, without building one.
type fixedChain []Scope

func (c fixedChain) ChainFor(context.Context, authz.Resource) ([]Scope, error) { return c, nil }

func benchEngine() *Engine {
	src := staticSource{
		"user:u":   {{RoleID: "reader", Scope: folder2}},
		"group:g":  {{RoleID: "reader", Scope: folder1}},
		"group:g2": {{RoleID: "editor", Scope: org1}},
	}
	pass := RuleFunc(func(context.Context, *principal.Principal, string, string, authz.Resource) (Decision, error) {
		return Continue, nil
	})
	return &Engine{Source: src, Scopes: fixedChain{folder1, org1, global}, Rules: []ProductRule{pass}, Global: global}
}

func BenchmarkAuthorizeGroupBinding(b *testing.B) {
	e := benchEngine()
	ctx := context.Background()
	p := &principal.Principal{ID: "u", Subjects: []string{"user:u", "group:g"}}
	res := doc("f1")
	b.ReportAllocs()
	for b.Loop() {
		if err := e.Authorize(ctx, p, "documents.get", res); err != nil {
			b.Fatal(err)
		}
	}
}

// A denial scans every subject's bindings.
func BenchmarkAuthorizeDenied(b *testing.B) {
	e := benchEngine()
	ctx := context.Background()
	p := &principal.Principal{ID: "u", Subjects: []string{"user:u", "group:g"}}
	res := doc("f1")
	b.ReportAllocs()
	for b.Loop() {
		if err := e.Authorize(ctx, p, "documents.delete", res); err == nil {
			b.Fatal("delete allowed")
		}
	}
}
