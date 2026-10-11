package principal

import (
	"context"
	"testing"
)

func TestAuthenticated(t *testing.T) {
	tests := []struct {
		name string
		p    *Principal
		want bool
	}{
		{"nil", nil, false},
		{"empty", &Principal{}, false},
		{"identity without an account id", &Principal{Issuer: "https://idp", Subject: "s"}, false},
		{"account", &Principal{ID: "u"}, true},
		{"admin", &Principal{Kind: KindAdmin, Admin: true}, true},
	}
	for _, tt := range tests {
		if got := tt.p.Authenticated(); got != tt.want {
			t.Errorf("%s: Authenticated() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestRuleAllows(t *testing.T) {
	r := Rule{Kinds: []string{"documents"}, Verbs: []string{"get", "list"}}
	wild := Rule{Kinds: []string{Wildcard}, Verbs: []string{Wildcard}}
	tests := []struct {
		rule       Rule
		kind, verb string
		want       bool
	}{
		{r, "documents", "get", true},
		{r, "documents", "delete", false},
		{r, "notes", "get", false},
		{wild, "notes", "delete", true},
		{Rule{}, "documents", "get", false},
	}
	for _, tt := range tests {
		if got := tt.rule.Allows(tt.kind, tt.verb); got != tt.want {
			t.Errorf("%+v.Allows(%s, %s) = %v, want %v", tt.rule, tt.kind, tt.verb, got, tt.want)
		}
	}
}

func TestContext(t *testing.T) {
	ctx := context.Background()
	if From(ctx) != nil {
		t.Fatal("empty context carries a principal")
	}
	p := &Principal{ID: "u"}
	if got := From(With(ctx, p)); got != p {
		t.Fatalf("From(With(p)) = %p, want %p", got, p)
	}
}
