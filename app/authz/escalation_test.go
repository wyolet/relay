package authz_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

// builtinRole returns the named built-in rule set.
func builtinRole(t *testing.T, name string) *role.Role {
	t.Helper()
	builtins, err := role.Builtins()
	if err != nil {
		t.Fatalf("built-in roles: %v", err)
	}
	for _, r := range builtins {
		if r.Meta.Name == name {
			r.Meta.ID = ids.New()
			return r
		}
	}
	t.Fatalf("no built-in role %q", name)
	return nil
}

// A binder must already hold everything the role grants at the binding's
// scope; the refusal names the first permission they lack.
func TestCheckGrant(t *testing.T) {
	cat := newFixture(t)
	r := authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}
	alice := ctxOf(actorOf(aliceID, subjectsOf(aliceID))) // team-admin on T1
	bob := ctxOf(actorOf(bobID, subjectsOf(bobID)))       // developer on P1
	admin := ctxOf(&actor.Actor{AdminToken: true})
	custom := func(name string, rules ...role.Rule) *role.Role {
		return &role.Role{Meta: meta.Metadata{ID: ids.New(), Name: name}, Spec: role.Spec{Rules: rules}}
	}
	rule := func(kinds, verbs string) role.Rule {
		return role.Rule{Kinds: strings.Split(kinds, ","), Verbs: strings.Split(verbs, ",")}
	}
	t1, t2, p1 := teamScope(t1ID), teamScope(t2ID), projectScope(p1ID)
	// No built-in below admin grants the vocabulary's first verb on keys.
	firstVerb := role.Verbs[slices.IndexFunc(role.Verbs, func(v string) bool { return v != role.Wildcard })]

	tests := []struct {
		name    string
		ctx     context.Context
		a       authz.Authorizer
		role    *role.Role
		scope   meta.Owner
		refused string // the permission named, "" = allowed
	}{
		{"own role at own team", alice, r, builtinRole(t, "team-admin"), t1, ""},
		{"own role at a project of own team", alice, r, builtinRole(t, "team-admin"), p1, ""},
		{"own role at a foreign team", alice, r, custom("reader", rule("keys", "get")), t2, "keys.get"},
		{"wider role at own team", alice, r, custom("wide", rule("keys", "get"), rule("settings", "update")), t1, "settings.update"},
		{"second verb of a rule", bob, r, custom("writer", rule("policies", "get,update,delete")), p1, "policies.update"},
		{"second kind of a rule", bob, r, custom("writer", rule("keys,policies", "create")), p1, "policies.create"},
		{"wildcard verb expands to the vocabulary", bob, r, custom("all-keys", rule("keys", "*")), p1, "keys." + firstVerb},
		{"role without rules grants nothing", bob, r, custom("empty"), t2, ""},
		{"admin binds anything", admin, r, builtinRole(t, "admin"), t1, ""},
		{"no authorizer is a loader", bob, nil, builtinRole(t, "admin"), t1, ""},
		{"no role", bob, r, nil, t1, ""},
		{"single-user mode holds everything", bob, authz.AlwaysAllowAuthenticated{}, builtinRole(t, "admin"), t1, ""},
		{"unauthenticated holds nothing", context.Background(), r, custom("reader", rule("keys", "get")), t1, "keys.get"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := authz.CheckGrant(tt.ctx, tt.a, tt.role, tt.scope)
			if tt.refused == "" {
				if err != nil {
					t.Fatalf("CheckGrant = %v, want allowed", err)
				}
				return
			}
			want := fmt.Sprintf("%v: binding role %q would grant %s, which you do not hold at this scope", authz.ErrForbidden, tt.role.Meta.Name, tt.refused)
			if !errors.Is(err, authz.ErrForbidden) || err.Error() != want {
				t.Fatalf("CheckGrant = %v, want %q", err, want)
			}
		})
	}
}
