package rbac

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/auth/authz"
)

var (
	global  = Scope{Kind: "global"}
	org1    = Scope{Kind: "org", ID: "o1"}
	folder1 = Scope{Kind: "folder", ID: "f1"}
	folder2 = Scope{Kind: "folder", ID: "f2"}
	errDown = errors.New("store down")
)

// memSource is an in-memory Source that counts lookups per subject.
type memSource struct {
	bindings   map[string][]Binding
	roles      map[string]Role
	bindingErr error
	roleErr    error
	asked      map[string]int
}

func (s *memSource) BindingsForSubject(_ context.Context, subject string) ([]Binding, error) {
	if s.asked == nil {
		s.asked = map[string]int{}
	}
	s.asked[subject]++
	return s.bindings[subject], s.bindingErr
}

func (s *memSource) Role(_ context.Context, id string) (Role, bool, error) {
	r, ok := s.roles[id]
	return r, ok, s.roleErr
}

// folderChains puts a document owned by a folder in [folder, org1, global]; a folder resource is its own innermost scope.
type folderChains struct{ err error }

func (c folderChains) ChainFor(_ context.Context, res authz.Resource) ([]Scope, error) {
	if c.err != nil {
		return nil, c.err
	}
	if res.Kind == "folder" && res.ID != "" {
		return []Scope{{Kind: "folder", ID: res.ID}, org1, global}, nil
	}
	if res.Owner != nil && res.Owner.Kind == "folder" {
		return []Scope{{Kind: "folder", ID: res.Owner.ID}, org1, global}, nil
	}
	if res.Owner != nil && res.Owner.Kind == "org" {
		return []Scope{{Kind: "org", ID: res.Owner.ID}, global}, nil
	}
	return []Scope{global}, nil
}

var roles = map[string]Role{
	"reader": {ID: "reader", Rules: []Rule{{Kinds: []string{"documents"}, Verbs: []string{"get", "list"}}}},
	"editor": {ID: "editor", Rules: []Rule{{Kinds: []string{"documents", "folders"}, Verbs: []string{"*"}}}},
	"anyget": {ID: "anyget", Rules: []Rule{{Kinds: []string{"*"}, Verbs: []string{"get"}}}},
	"owner":  {ID: "owner", Rules: []Rule{{Kinds: []string{"*"}, Verbs: []string{"*"}}}},
}

func newEngine(bindings map[string][]Binding, rules ...ProductRule) (*Engine, *memSource) {
	src := &memSource{bindings: bindings, roles: roles}
	return &Engine{Source: src, Scopes: folderChains{}, Rules: rules, Global: global}, src
}

func doc(folder string) authz.Resource {
	return authz.Resource{Kind: "document", ID: "d-" + folder, Owner: &authz.Owner{Kind: "folder", ID: folder}}
}

func user(id string, subjects ...string) *Principal { return &Principal{ID: id, Subjects: subjects} }

func TestAuthorizeRefusesUnauthenticated(t *testing.T) {
	e, _ := newEngine(nil)
	for name, p := range map[string]*Principal{
		"nil":                    nil,
		"empty":                  {},
		"subjects without an id": {Subjects: []string{"user:u"}},
	} {
		if err := e.Authorize(context.Background(), p, "documents.get", doc("f1")); !errors.Is(err, authz.ErrUnauthenticated) {
			t.Errorf("%s: %v, want unauthenticated", name, err)
		}
	}
}

func TestBindingsFollowTheScopeChain(t *testing.T) {
	e, _ := newEngine(map[string][]Binding{
		"user:folder-reader": {{RoleID: "reader", Scope: folder1}},
		"user:org-editor":    {{RoleID: "editor", Scope: org1}},
		"user:global-anyget": {{RoleID: "anyget", Scope: global}},
		"user:unknown-role":  {{RoleID: "missing", Scope: global}},
		"group:readers":      {{RoleID: "reader", Scope: folder2}},
	})
	ctx := context.Background()
	tests := []struct {
		name   string
		p      *Principal
		action string
		res    authz.Resource
		want   error
	}{
		{"binding at the resource's scope", user("u1", "user:folder-reader"), "documents.get", doc("f1"), nil},
		{"binding at a sibling scope", user("u1", "user:folder-reader"), "documents.get", doc("f2"), authz.ErrForbidden},
		{"role does not cover the verb", user("u1", "user:folder-reader"), "documents.delete", doc("f1"), authz.ErrForbidden},
		{"outer scope reaches inner", user("u2", "user:org-editor"), "documents.delete", doc("f2"), nil},
		{"wildcard verb", user("u2", "user:org-editor"), "folders.archive", doc("f1"), nil},
		{"wildcard kind", user("u3", "user:global-anyget"), "anything.get", doc("f9"), nil},
		{"sub-resource action", user("u3", "user:global-anyget"), "documents.history.get", doc("f9"), nil},
		{"unknown role", user("u4", "user:unknown-role"), "documents.get", doc("f1"), authz.ErrForbidden},
		{"second subject", user("u5", "user:nobody", "group:readers"), "documents.get", doc("f2"), nil},
		{"a resource is its own scope", user("u1", "user:folder-reader"), "documents.get",
			authz.Resource{Kind: "folder", ID: "f1", Owner: &authz.Owner{Kind: "org", ID: "o1"}}, nil},
		{"no binding", user("u6"), "documents.get", doc("f1"), authz.ErrForbidden},
		{"admin", &Principal{Admin: true}, "anything.delete", authz.Resource{}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := e.Authorize(ctx, tt.p, tt.action, tt.res); err != tt.want {
				t.Fatalf("Authorize(%s) = %v, want %v", tt.action, err, tt.want)
			}
		})
	}
}

// A list on no resource is admitted by a binding at any scope; the caller then filters rows through Visible.
func TestListWithoutOwnerTakesAnyScope(t *testing.T) {
	e, _ := newEngine(map[string][]Binding{"user:r": {{RoleID: "reader", Scope: folder1}}})
	ctx := context.Background()
	p := user("u", "user:r")
	if err := e.Authorize(ctx, p, "documents.list", authz.Resource{Kind: "document"}); err != nil {
		t.Fatalf("list without owner = %v, want allowed", err)
	}
	if err := e.Authorize(ctx, p, "documents.get", authz.Resource{Kind: "document"}); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("get without owner = %v, want forbidden", err)
	}
	if err := e.Authorize(ctx, p, "documents.list", doc("f2")); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("list on an owned resource elsewhere = %v, want forbidden", err)
	}
}

func TestReservedAuthenticatedSubject(t *testing.T) {
	e, src := newEngine(map[string][]Binding{SubjectAuthenticated: {{RoleID: "reader", Scope: global}}})
	ctx := context.Background()
	if err := e.Authorize(ctx, user("u"), "documents.get", doc("f1")); err != nil {
		t.Fatalf("all-authenticated binding = %v, want allowed", err)
	}
	src.asked = nil
	if err := e.Authorize(ctx, user("u", SubjectAuthenticated), "documents.delete", doc("f1")); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("delete = %v, want forbidden", err)
	}
	if src.asked[SubjectAuthenticated] != 1 {
		t.Fatalf("reserved subject asked %d times, want once", src.asked[SubjectAuthenticated])
	}
}

func TestProductRulesRunInOrderBeforeBindings(t *testing.T) {
	var calls []string
	rule := func(name string, d Decision, err error) ProductRule {
		return RuleFunc(func(context.Context, *Principal, string, string, authz.Resource) (Decision, error) {
			calls = append(calls, name)
			return d, err
		})
	}
	bound := map[string][]Binding{"user:r": {{RoleID: "reader", Scope: global}}}
	ctx := context.Background()
	p := user("u", "user:r")

	tests := []struct {
		name  string
		rules []ProductRule
		want  error
		calls []string
	}{
		{"allow wins without a binding", []ProductRule{rule("a", Continue, nil), rule("b", Allow, nil), rule("c", Deny, nil)},
			nil, []string{"a", "b"}},
		{"deny beats a binding", []ProductRule{rule("a", Deny, nil), rule("b", Allow, nil)}, authz.ErrForbidden, []string{"a"}},
		{"continue reaches the bindings", []ProductRule{rule("a", Continue, nil)}, nil, []string{"a"}},
		{"an error is not a denial", []ProductRule{rule("a", Continue, errDown), rule("b", Allow, nil)}, errDown, []string{"a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls = nil
			e, _ := newEngine(bound, tt.rules...)
			if err := e.Authorize(ctx, p, "documents.get", doc("f1")); err != tt.want {
				t.Fatalf("Authorize = %v, want %v", err, tt.want)
			}
			if len(calls) != len(tt.calls) {
				t.Fatalf("rules called %v, want %v", calls, tt.calls)
			}
		})
	}
}

func TestLookupFailuresAreErrors(t *testing.T) {
	ctx := context.Background()
	p := user("u", "user:r")
	bound := map[string][]Binding{"user:r": {{RoleID: "reader", Scope: global}}}

	e, src := newEngine(bound)
	src.bindingErr = errDown
	if err := e.Authorize(ctx, p, "documents.get", doc("f1")); err != errDown {
		t.Errorf("binding lookup failure = %v", err)
	}
	e, src = newEngine(bound)
	src.roleErr = errDown
	if err := e.Authorize(ctx, p, "documents.get", doc("f1")); err != errDown {
		t.Errorf("role lookup failure = %v", err)
	}
	e, _ = newEngine(bound)
	e.Scopes = folderChains{err: errDown}
	if err := e.Authorize(ctx, p, "documents.get", doc("f1")); err != errDown {
		t.Errorf("chain failure = %v", err)
	}
	ok, err := e.Allowed(ctx, p, "documents.get", doc("f1"))
	if ok || err != errDown {
		t.Errorf("Allowed on a failure = %v, %v", ok, err)
	}
	if e.Visible(ctx, p, "documents", doc("f1")) {
		t.Error("a failed lookup made a resource visible")
	}
}

func TestMissingSourceOrResolverDenies(t *testing.T) {
	ctx := context.Background()
	p := user("u", "user:r")
	bound := map[string][]Binding{"user:r": {{RoleID: "reader", Scope: global}}}
	e, _ := newEngine(bound)
	e.Scopes = nil
	if err := e.Authorize(ctx, p, "documents.get", doc("f1")); !errors.Is(err, authz.ErrForbidden) {
		t.Errorf("no resolver = %v, want forbidden", err)
	}
	if err := e.Authorize(ctx, p, "documents.list", authz.Resource{}); err != nil {
		t.Errorf("unowned list needs no resolver: %v", err)
	}
	e.Source = nil
	if err := e.Authorize(ctx, p, "documents.list", authz.Resource{}); !errors.Is(err, authz.ErrForbidden) {
		t.Errorf("no source = %v, want forbidden", err)
	}
}

func TestAllowedAndVisible(t *testing.T) {
	e, _ := newEngine(map[string][]Binding{"user:r": {{RoleID: "reader", Scope: folder1}}})
	ctx := context.Background()
	p := user("u", "user:r")
	for _, tt := range []struct {
		p      *Principal
		action string
		res    authz.Resource
		want   bool
	}{
		{p, "documents.get", doc("f1"), true},
		{p, "documents.get", doc("f2"), false},
		{nil, "documents.get", doc("f1"), false},
	} {
		ok, err := e.Allowed(ctx, tt.p, tt.action, tt.res)
		if err != nil || ok != tt.want {
			t.Errorf("Allowed(%s, %+v) = %v, %v; want %v", tt.action, tt.res.Owner, ok, err, tt.want)
		}
		if got := e.Visible(ctx, tt.p, "documents", tt.res); got != tt.want {
			t.Errorf("Visible(%+v) = %v, want %v", tt.res.Owner, got, tt.want)
		}
	}
}

func TestCredentialScopeNarrowsEveryAllow(t *testing.T) {
	ctx := context.Background()
	allowAll := RuleFunc(func(_ context.Context, _ *Principal, kind, _ string, _ authz.Resource) (Decision, error) {
		if kind == "notes" {
			return Allow, nil
		}
		return Continue, nil
	})
	e, _ := newEngine(map[string][]Binding{"user:o": {{RoleID: "owner", Scope: global}}}, allowAll)
	readDocs := []Rule{{Kinds: []string{"documents", "notes"}, Verbs: []string{"get", "list", "create"}}}
	note := authz.Resource{Kind: "note", ID: "n1"}
	within := func(kind string, scopes ...Scope) *CredentialScope {
		return &CredentialScope{Rules: readDocs, Within: map[string][]Scope{kind: scopes}}
	}
	noteIn := func(folder string) authz.Resource {
		return authz.Resource{Kind: "note", ID: "n-" + folder, Owner: &authz.Owner{Kind: "folder", ID: folder}}
	}
	unowned := authz.Resource{Kind: "document"}

	tests := []struct {
		name   string
		cred   *CredentialScope
		admin  bool
		action string
		res    authz.Resource
		want   error
	}{
		{"unrestricted", nil, false, "documents.delete", doc("f1"), nil},
		{"capability covers", &CredentialScope{Rules: readDocs}, false, "documents.get", doc("f1"), nil},
		{"capability refuses a binding", &CredentialScope{Rules: readDocs}, false, "documents.delete", doc("f1"), authz.ErrForbidden},
		{"capability refuses a product rule", &CredentialScope{Rules: readDocs}, false, "notes.delete", note, authz.ErrForbidden},
		{"capability refuses an admin", &CredentialScope{Rules: readDocs}, true, "documents.delete", doc("f1"), authz.ErrForbidden},
		{"no rules permit nothing", &CredentialScope{}, false, "documents.get", doc("f1"), authz.ErrForbidden},
		{"a parent-scope limit reaches a child", within("documents", org1), false, "documents.get", doc("f2"), nil},
		{"a scope limit reaches its own resource", within("documents", folder1), false, "documents.get", doc("f1"), nil},
		{"a sibling scope is refused", within("documents", folder1), false, "documents.get", doc("f2"), authz.ErrForbidden},
		{"listed scopes of mixed kinds", within("documents", Scope{Kind: "org", ID: "o9"}, folder2), false, "documents.get", doc("f2"), nil},
		{"an empty list reaches nothing", within("documents"), false, "documents.get", doc("f1"), authz.ErrForbidden},
		{"an empty list admits no list", within("documents"), false, "documents.list", unowned, authz.ErrForbidden},
		{"an unowned list is admitted", within("documents", folder1), false, "documents.list", unowned, nil},
		{"an owned list is limited", within("documents", folder1), false, "documents.list", doc("f2"), authz.ErrForbidden},
		{"create with no id and no owner is refused", within("documents", folder1), false, "documents.create", unowned, authz.ErrForbidden},
		{"get with no id and no owner is refused", within("documents", folder1), false, "documents.get", unowned, authz.ErrForbidden},
		{"create inside the limit", within("documents", folder1), false, "documents.create",
			authz.Resource{Kind: "document", Owner: &authz.Owner{Kind: "folder", ID: "f1"}}, nil},
		{"admin inside the limit", within("documents", folder1), true, "documents.get", doc("f1"), nil},
		{"admin outside the limit", within("documents", folder1), true, "documents.get", doc("f2"), authz.ErrForbidden},
		{"product rule inside the limit", within("notes", folder1), false, "notes.get", noteIn("f1"), nil},
		{"product rule outside the limit", within("notes", folder1), false, "notes.get", noteIn("f2"), authz.ErrForbidden},
		{"other kind not limited", within("documents"), false, "notes.get", note, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Principal{ID: "u", Subjects: []string{"user:o"}, Admin: tt.admin, Credential: tt.cred}
			if err := e.Authorize(ctx, p, tt.action, tt.res); err != tt.want {
				t.Fatalf("Authorize(%s) = %v, want %v", tt.action, err, tt.want)
			}
		})
	}
}

// countingChains counts ChainFor calls, so a test can see the chain resolved once per decision.
type countingChains struct {
	folderChains
	calls *int
}

func (c countingChains) ChainFor(ctx context.Context, res authz.Resource) ([]Scope, error) {
	*c.calls++
	return c.folderChains.ChainFor(ctx, res)
}

func TestCredentialScopeResolvesTheChainOnce(t *testing.T) {
	ctx := context.Background()
	cred := &CredentialScope{Rules: []Rule{{Kinds: []string{Wildcard}, Verbs: []string{Wildcard}}}, Within: map[string][]Scope{"documents": {folder1}}}
	e, _ := newEngine(map[string][]Binding{"user:o": {{RoleID: "owner", Scope: global}}})
	calls := 0
	e.Scopes = countingChains{calls: &calls}

	if err := e.Authorize(ctx, &Principal{ID: "u", Subjects: []string{"user:o"}, Credential: cred}, "documents.get", doc("f1")); err != nil || calls != 1 {
		t.Fatalf("binding path: %v after %d chain lookups, want allowed after 1", err, calls)
	}
	calls = 0
	if err := e.Authorize(ctx, &Principal{Admin: true, Credential: cred}, "documents.get", doc("f1")); err != nil || calls != 1 {
		t.Fatalf("admin path: %v after %d chain lookups, want allowed after 1", err, calls)
	}

	e.Scopes = folderChains{err: errDown}
	if err := e.Authorize(ctx, &Principal{Admin: true, Credential: cred}, "documents.get", doc("f1")); err != errDown {
		t.Fatalf("chain failure in the credential check = %v, want it returned", err)
	}
	e.Scopes = nil
	if err := e.Authorize(ctx, &Principal{Admin: true, Credential: cred}, "documents.get", doc("f1")); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("no resolver = %v, want forbidden", err)
	}
}

// A limited credential may call the list; the rows it gets back are the ones inside the limit.
func TestLimitedListIsAdmittedThenFiltered(t *testing.T) {
	ctx := context.Background()
	e, _ := newEngine(map[string][]Binding{"user:o": {{RoleID: "owner", Scope: global}}})
	p := &Principal{ID: "u", Subjects: []string{"user:o"}, Credential: &CredentialScope{
		Rules:  []Rule{{Kinds: []string{"documents"}, Verbs: []string{"get", "list", "create"}}},
		Within: map[string][]Scope{"documents": {folder1}},
	}}
	if err := e.Authorize(ctx, p, "documents.list", authz.Resource{Kind: "document"}); err != nil {
		t.Fatalf("list = %v, want admitted", err)
	}
	if !e.Visible(ctx, p, "documents", doc("f1")) || e.Visible(ctx, p, "documents", doc("f2")) {
		t.Fatal("rows outside the limit survive the filter, or rows inside it do not")
	}
	f, err := e.ScopeOf(ctx, p, "get", "documents")
	if err != nil || !f.All || len(f.Within) != 1 || f.Within[0] != folder1 {
		t.Fatalf("ScopeOf = %+v, %v; want everything within folder1", f, err)
	}
	if err := e.Authorize(ctx, p, "documents.create", authz.Resource{Kind: "document"}); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("create without an owner = %v, want forbidden", err)
	}
}

func TestMatchesSubject(t *testing.T) {
	p := user("u", "user:u", "group:g")
	for _, tt := range []struct {
		p       *Principal
		subject string
		want    bool
	}{
		{p, "user:u", true},
		{p, "group:g", true},
		{p, "group:other", false},
		{p, SubjectAuthenticated, true},
		{&Principal{Admin: true}, SubjectAuthenticated, true},
		{nil, SubjectAuthenticated, false},
		{&Principal{Subjects: []string{"user:u"}}, "user:u", false},
	} {
		if got := MatchesSubject(tt.p, tt.subject); got != tt.want {
			t.Errorf("MatchesSubject(%+v, %q) = %v, want %v", tt.p, tt.subject, got, tt.want)
		}
	}
}

func TestSplitAction(t *testing.T) {
	for _, tt := range []struct{ action, kind, verb string }{
		{"documents.get", "documents", "get"},
		{"documents.history.get", "documents", "get"},
		{"documents", "documents", "documents"},
		{"documents.", "documents", ""},
		{".get", "", "get"},
		{"", "", ""},
	} {
		kind, verb := SplitAction(tt.action)
		if kind != tt.kind || verb != tt.verb {
			t.Errorf("SplitAction(%q) = %q, %q; want %q, %q", tt.action, kind, verb, tt.kind, tt.verb)
		}
	}
}

func TestRuleMatching(t *testing.T) {
	r := Role{Rules: []Rule{
		{Kinds: []string{"documents"}, Verbs: []string{"get"}},
		{Kinds: []string{Wildcard}, Verbs: []string{"list"}},
	}}
	for _, tt := range []struct {
		kind, verb string
		want       bool
	}{
		{"documents", "get", true},
		{"documents", "delete", false},
		{"folders", "list", true},
		{"folders", "get", false},
		{"documents", Wildcard, false},
	} {
		if got := r.Allows(tt.kind, tt.verb); got != tt.want {
			t.Errorf("Allows(%s, %s) = %v, want %v", tt.kind, tt.verb, got, tt.want)
		}
	}
}
