package rbac

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/principal"
)

type namedRule struct {
	RuleFunc
	name string
}

func (r namedRule) Name() string { return r.name }

func ruleFor(kind string, d Decision) RuleFunc {
	return func(_ context.Context, _ *principal.Principal, k, _ string, _ authz.Resource) (Decision, error) {
		if k == kind {
			return d, nil
		}
		return Continue, nil
	}
}

func TestExplainReasons(t *testing.T) {
	ctx := context.Background()
	e, _ := newEngine(map[string][]Binding{
		"user:folder-reader":  {{RoleID: "reader", Scope: folder1}},
		"group:readers":       {{RoleID: "anyget", Scope: folder2}, {RoleID: "reader", Scope: folder2}},
		SubjectAuthenticated:  {{RoleID: "reader", Scope: Scope{Kind: "folder", ID: "f3"}}},
		"user:global-editors": {{RoleID: "editor", Scope: global}},
	},
		namedRule{ruleFor("notes", Allow), "own notes"},
		namedRule{ruleFor("secrets", Deny), "no secrets"},
		ruleFor("drafts", Allow),
	)
	readOnly := &principal.CredentialScope{Rules: []Rule{{Kinds: []string{"documents"}, Verbs: []string{"get"}}}}
	chain := func(folder string) []Scope { return []Scope{{Kind: "folder", ID: folder}, org1, global} }

	tests := []struct {
		name   string
		p      *principal.Principal
		action string
		res    authz.Resource
		want   Explanation
	}{
		{"admin", &principal.Principal{Admin: true}, "documents.delete", doc("f1"),
			Explanation{Allowed: true, Reason: ReasonAdmin}},
		{"named rule allows", user("u"), "notes.get", doc("f1"),
			Explanation{Allowed: true, Reason: ReasonRule, Rule: "own notes"}},
		{"named rule denies", user("u", "user:global-editors"), "secrets.get", doc("f1"),
			Explanation{Reason: ReasonRule, Rule: "no secrets"}},
		{"unnamed rule", user("u"), "drafts.get", doc("f1"),
			Explanation{Allowed: true, Reason: ReasonRule}},
		{"direct binding", user("u", "user:folder-reader"), "documents.get", doc("f1"),
			Explanation{Allowed: true, Reason: ReasonBinding, Subject: "user:folder-reader", RoleID: "reader", Scope: folder1, Chain: chain("f1")}},
		{"group binding, first covering role", user("u", "user:nobody", "group:readers"), "documents.list", doc("f2"),
			Explanation{Allowed: true, Reason: ReasonBinding, Subject: "group:readers", RoleID: "reader", Scope: folder2, Chain: chain("f2")}},
		{"outer scope", user("u", "user:global-editors"), "documents.delete", doc("f2"),
			Explanation{Allowed: true, Reason: ReasonBinding, Subject: "user:global-editors", RoleID: "editor", Scope: global, Chain: chain("f2")}},
		{"all-authenticated subject", user("u"), "documents.get", doc("f3"),
			Explanation{Allowed: true, Reason: ReasonBinding, Subject: SubjectAuthenticated, RoleID: "reader", Scope: Scope{Kind: "folder", ID: "f3"}, Chain: chain("f3")}},
		{"unowned list takes no chain", user("u", "user:folder-reader"), "documents.list", authz.Resource{Kind: "document"},
			Explanation{Allowed: true, Reason: ReasonBinding, Subject: "user:folder-reader", RoleID: "reader", Scope: folder1}},
		{"credential refuses a binding",
			&principal.Principal{ID: "u", Subjects: []string{"user:global-editors"}, Credential: principal.Credential{Scope: readOnly}}, "documents.delete", doc("f1"),
			Explanation{Reason: ReasonCredential, CredentialDenied: true, Subject: "user:global-editors", RoleID: "editor", Scope: global, Chain: chain("f1")}},
		{"credential refuses a rule",
			&principal.Principal{ID: "u", Credential: principal.Credential{Scope: readOnly}}, "notes.get", doc("f1"),
			Explanation{Reason: ReasonCredential, CredentialDenied: true, Rule: "own notes"}},
		{"credential refuses an admin",
			&principal.Principal{Admin: true, Credential: principal.Credential{Scope: readOnly}}, "documents.delete", doc("f1"),
			Explanation{Reason: ReasonCredential, CredentialDenied: true}},
		{"credential admits", &principal.Principal{ID: "u", Subjects: []string{"user:global-editors"}, Credential: principal.Credential{Scope: readOnly}}, "documents.get", doc("f1"),
			Explanation{Allowed: true, Reason: ReasonBinding, Subject: "user:global-editors", RoleID: "editor", Scope: global, Chain: chain("f1")}},
		{"none", user("u", "user:folder-reader"), "documents.delete", doc("f1"),
			Explanation{Reason: ReasonNone, Chain: chain("f1")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := e.Explain(ctx, tt.p, tt.action, tt.res)
			if err != nil {
				t.Fatalf("Explain: %v", err)
			}
			if !equalExplanation(got, tt.want) {
				t.Fatalf("Explain(%s) = %+v, want %+v", tt.action, got, tt.want)
			}
			if authorized := e.Authorize(ctx, tt.p, tt.action, tt.res) == nil; authorized != got.Allowed {
				t.Fatalf("Authorize allowed = %v, Explain allowed = %v", authorized, got.Allowed)
			}
		})
	}
}

func equalExplanation(a, b Explanation) bool {
	return a.Allowed == b.Allowed && a.Reason == b.Reason && a.Rule == b.Rule &&
		a.Subject == b.Subject && a.RoleID == b.RoleID && a.Scope == b.Scope &&
		a.CredentialDenied == b.CredentialDenied && slices.Equal(a.Chain, b.Chain)
}

func TestExplainErrorsAreAuthorizes(t *testing.T) {
	ctx := context.Background()
	e, src := newEngine(map[string][]Binding{"user:r": {{RoleID: "reader", Scope: global}}})
	if _, err := e.Explain(ctx, nil, "documents.get", doc("f1")); !errors.Is(err, authz.ErrUnauthenticated) {
		t.Fatalf("unauthenticated = %v", err)
	}
	src.bindingErr = errDown
	if x, err := e.Explain(ctx, user("u", "user:r"), "documents.get", doc("f1")); err != errDown || x.Allowed {
		t.Fatalf("failed lookup = %+v, %v; want the error and no allow", x, err)
	}
}

// reusedChain hands out the same buffer on every call, as relay's resolver does.
type reusedChain struct{ buf []Scope }

func (c *reusedChain) ChainFor(_ context.Context, res authz.Resource) ([]Scope, error) {
	c.buf = append(c.buf[:0], Scope{Kind: "folder", ID: res.Owner.ID}, global)
	return c.buf, nil
}

func TestExplainChainOutlivesTheResolverBuffer(t *testing.T) {
	ctx := context.Background()
	e, _ := newEngine(nil)
	chains := &reusedChain{}
	e.Scopes = chains
	x, err := e.Explain(ctx, user("u"), "documents.get", doc("f1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chains.ChainFor(ctx, doc("f9")); err != nil {
		t.Fatal(err)
	}
	if x.Chain[0] != folder1 {
		t.Fatalf("chain = %+v, changed under the explanation", x.Chain)
	}
}

// Every principal, action and resource of the engine tests, through both entry points.
func TestExplainAgreesWithAuthorize(t *testing.T) {
	ctx := context.Background()
	e, _ := newEngine(map[string][]Binding{
		"user:r":             {{RoleID: "reader", Scope: folder1}},
		"user:o":             {{RoleID: "owner", Scope: global}},
		"group:g":            {{RoleID: "editor", Scope: org1}},
		SubjectAuthenticated: {{RoleID: "anyget", Scope: folder2}},
	}, ruleFor("notes", Allow), ruleFor("secrets", Deny))
	creds := []*principal.CredentialScope{
		nil,
		{},
		{Rules: []Rule{{Kinds: []string{"documents", "notes"}, Verbs: []string{"get", "list"}}}},
		{Rules: []Rule{{Kinds: []string{Wildcard}, Verbs: []string{Wildcard}}}, Within: map[string][]Scope{"documents": {folder1}}},
		{Rules: []Rule{{Kinds: []string{Wildcard}, Verbs: []string{Wildcard}}}, Within: map[string][]Scope{"documents": {}}},
	}
	var principals []*principal.Principal
	for _, c := range creds {
		for _, subs := range [][]string{nil, {"user:r"}, {"user:o"}, {"user:x", "group:g"}} {
			principals = append(principals, &principal.Principal{ID: "u", Subjects: subs, Credential: principal.Credential{Scope: c}})
		}
		principals = append(principals, &principal.Principal{Admin: true, Credential: principal.Credential{Scope: c}})
	}
	principals = append(principals, nil, &principal.Principal{})
	resources := []authz.Resource{
		{}, {Kind: "document"}, doc("f1"), doc("f2"), doc("f9"),
		{Kind: "folder", ID: "f1"}, {Kind: "document", Owner: &authz.Owner{Kind: "org", ID: "o1"}},
	}
	n := 0
	for _, p := range principals {
		for _, kind := range []string{"documents", "folders", "notes", "secrets", "other"} {
			for _, verb := range []string{"get", "list", "create", "delete", "*"} {
				for _, res := range resources {
					action := kind + "." + verb
					want := e.Authorize(ctx, p, action, res)
					x, err := e.Explain(ctx, p, action, res)
					n++
					switch {
					case want == nil && (!x.Allowed || err != nil),
						errors.Is(want, authz.ErrForbidden) && (x.Allowed || err != nil),
						want != nil && !errors.Is(want, authz.ErrForbidden) && err != want:
						t.Fatalf("%+v %s on %+v: Authorize %v, Explain %+v / %v", p, action, res, want, x, err)
					}
				}
			}
		}
	}
	t.Logf("%d cases agree", n)
}
