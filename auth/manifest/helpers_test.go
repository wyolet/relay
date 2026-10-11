package manifest_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/directory/memstore"
	"github.com/wyolet/relay/auth/manifest"
	"github.com/wyolet/relay/auth/principal"
	"github.com/wyolet/relay/auth/rbac"
)

// allowAll passes every grant check; the grant checks themselves are the directory's to test.
type allowAll struct{}

func (allowAll) CheckGrant(context.Context, *principal.Principal, rbac.Role, rbac.Scope, []string, []string) error {
	return nil
}

// denyBindings refuses every grant below the global scope, which in the test bundle is the project binding.
type denyBindings struct{ called bool }

func (d *denyBindings) CheckGrant(_ context.Context, _ *principal.Principal, _ rbac.Role, scope rbac.Scope, _, _ []string) error {
	if scope == directory.Global {
		return nil
	}
	d.called = true
	return authz.ErrForbidden
}

var admin = &principal.Principal{ID: "operator", Admin: true}

// products is a stand-in for a product's own kind that a product field refers to.
type products map[string]string

func (p products) ID(kind, name string) (string, bool) {
	if kind != "Budget" {
		return "", false
	}
	id, ok := p[name]
	return id, ok
}

func (p products) Name(kind, id string) (string, bool) {
	for n, v := range p {
		if kind == "Budget" && v == id {
			return n, true
		}
	}
	return "", false
}

// testConfig registers a small vocabulary, a plain field on Team and a field on ServiceAccount that names a product row.
func testConfig() manifest.Config {
	return manifest.Config{
		RuleKinds: []string{"models", "keys", "teams"},
		RuleVerbs: []string{"get", "list", "update"},
		Fields: map[string][]manifest.Field{
			manifest.KindTeam: {{Name: "costCenter", Schema: map[string]any{"type": "string"}}},
			manifest.KindServiceAccount: {{
				Name: "budget", Attribute: "budgetId",
				ToAttribute: func(v json.RawMessage, refs manifest.Resolver) (json.RawMessage, error) {
					var name string
					if err := json.Unmarshal(v, &name); err != nil {
						return nil, err
					}
					id, ok := refs.ID("Budget", name)
					if !ok {
						return nil, &manifest.RefError{Kind: "budget", Name: name}
					}
					return json.Marshal(id)
				},
				FromAttribute: func(stored json.RawMessage, names manifest.Namer) (json.RawMessage, error) {
					var id string
					if err := json.Unmarshal(stored, &id); err != nil {
						return nil, err
					}
					if n, ok := names.Name("Budget", id); ok {
						return json.Marshal(n)
					}
					return json.Marshal(id)
				},
			}},
		},
		LookupEnv: func(name string) (string, bool) {
			v, ok := testEnv[name]
			return v, ok
		},
	}
}

var testEnv = map[string]string{"ALICE_PASSWORD": "correct horse battery"}

func testRegistry(t *testing.T) *manifest.Registry {
	t.Helper()
	r, err := manifest.NewRegistry(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type env struct {
	reg   *manifest.Registry
	store *memstore.Store
	dir   *directory.Directory
	opts  manifest.Options
}

func newEnv(t *testing.T) *env {
	t.Helper()
	store := memstore.New()
	cfg := testConfig()
	dir, err := directory.New(directory.Config{Stores: store.Stores(), Grants: allowAll{}, Kinds: cfg.RuleKinds, Verbs: cfg.RuleVerbs})
	if err != nil {
		t.Fatal(err)
	}
	refs := products{"q3": "budget-q3", "q4": "budget-q4"}
	return &env{reg: testRegistry(t), store: store, dir: dir, opts: manifest.Options{Refs: refs, Names: refs}}
}

func (e *env) parse(t *testing.T, src string) []manifest.Document {
	t.Helper()
	docs, err := e.reg.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return docs
}

func (e *env) plan(t *testing.T, docs []manifest.Document, opts manifest.Options) *manifest.Plan {
	t.Helper()
	opts.Refs, opts.Names = e.opts.Refs, e.opts.Names
	p, err := e.reg.Plan(context.Background(), e.store.Stores(), docs, opts)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return p
}

// apply plans src and applies it, returning the plan.
func (e *env) apply(t *testing.T, src string, opts manifest.Options) *manifest.Plan {
	t.Helper()
	p := e.plan(t, e.parse(t, src), opts)
	if _, err := p.Apply(context.Background(), e.dir, admin); err != nil {
		t.Fatalf("apply: %v", err)
	}
	return p
}

// actions renders a plan as "Action Kind/name" lines.
func actions(p *manifest.Plan) []string {
	out := make([]string, 0, len(p.Entries))
	for _, e := range p.Entries {
		line := string(e.Action) + " " + e.Kind + "/" + e.Name
		if len(e.ChangedFields) > 0 {
			line += " " + strings.Join(e.ChangedFields, ",")
		}
		out = append(out, line)
	}
	return out
}

func wantLines(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func asError(t *testing.T, err error) *manifest.Error {
	t.Helper()
	var e *manifest.Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v (%T), want a *manifest.Error", err, err)
	}
	return e
}
