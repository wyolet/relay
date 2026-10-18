package refcheck

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/user"
)

// Reading another project's host key is not a licence to spend it: a
// project's policy may name only its own keys or shared ones.
func TestProjectPolicyCannotSpendAnotherProjectsHostKey(t *testing.T) {
	keys := map[string]*hostkey.HostKey{
		"hk-own":    {Meta: meta.Metadata{ID: "hk-own", Name: "own", Owner: meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}}},
		"hk-other":  {Meta: meta.Metadata{ID: "hk-other", Name: "other", Owner: meta.Owner{Kind: meta.OwnerProject, ID: "p-2"}}},
		"hk-shared": {Meta: meta.Metadata{ID: "hk-shared", Name: "shared", Owner: meta.Owner{Kind: meta.OwnerSystem}}},
	}
	c := Checker{Rows: Lookup{HostKey: func(_ context.Context, id string) *hostkey.HostKey { return keys[id] }}}
	ref := meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}
	ctx := context.Background()
	if err := c.HostKeyRefs(ctx, []string{"hk-own", "hk-shared"}, ref); err != nil {
		t.Fatalf("own and shared keys: %v", err)
	}
	if err := c.HostKeyRefs(ctx, []string{"hk-other"}, ref); err == nil {
		t.Fatal("project p-1's policy accepted project p-2's host key")
	}
}

type scopingAuthz struct{}

func (scopingAuthz) Authorize(context.Context, string, authz.Resource) error { return nil }
func (scopingAuthz) Visible(context.Context, string, string, meta.Owner) bool {
	return true
}

// An env reference reads the relay's own process environment, so only an
// operator may write one; anyone else could read any variable through the
// key's health probe.
func TestEnvSourcedHostKeyNeedsAnAdmin(t *testing.T) {
	tier := &policy.Policy{Meta: meta.Metadata{ID: "tier", Name: "tier", Owner: meta.Owner{Kind: meta.OwnerHost, ID: "h-1"}}}
	c := Checker{Authz: scopingAuthz{}, Rows: Lookup{Policy: func(context.Context, string) *policy.Policy { return tier }}}
	k := &hostkey.HostKey{
		Meta: meta.Metadata{Name: "k", Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-1"}},
		Spec: hostkey.Spec{HostID: "h-1", PolicyID: "tier", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "HOME"}},
	}
	member := actor.WithActor(context.Background(), &actor.Actor{UserID: "u-1"})
	if err := c.HostKey(member, k); err == nil {
		t.Fatal("non-admin wrote an env-sourced host key")
	}
	admin := actor.WithActor(context.Background(), &actor.Actor{UserID: "u-2", Roles: []string{user.RoleAdmin}})
	if err := c.HostKey(admin, k); err != nil {
		t.Fatalf("admin: %v", err)
	}
	if err := (Checker{Rows: c.Rows}).HostKey(member, k); err != nil {
		t.Fatalf("unscoped authorizer (single-user): %v", err)
	}
}

// teamGrant allows a fixed action set at one team, nothing elsewhere.
type teamGrant struct {
	teamID  string
	actions map[string]bool
}

func (g teamGrant) Authorize(_ context.Context, action string, res authz.Resource) error {
	if res.Owner != nil && res.Owner.Kind == meta.OwnerTeam && res.Owner.ID == g.teamID && g.actions[action] {
		return nil
	}
	return authz.ErrForbidden
}

// Editing a role that is already bound changes what every binding grants,
// so the editor must hold the new rules at each bound scope — exactly what
// creating those bindings would have required.
func TestEditingABoundRoleCannotGrantMoreThanTheEditorHolds(t *testing.T) {
	bound := &rolebinding.RoleBinding{Spec: rolebinding.Spec{RoleID: "r-1", Scope: meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"}}}
	c := Checker{
		Authz: teamGrant{teamID: "t-1", actions: map[string]bool{"keys.get": true}},
		Rows: Lookup{RoleBindingsFor: func(_ context.Context, roleID string) ([]*rolebinding.RoleBinding, error) {
			if roleID == "r-1" {
				return []*rolebinding.RoleBinding{bound}, nil
			}
			return nil, nil
		}},
	}
	edit := func(kinds, verbs []string) *role.Role {
		return &role.Role{Meta: meta.Metadata{ID: "r-1", Name: "mine"}, Spec: role.Spec{Rules: []role.Rule{{Kinds: kinds, Verbs: verbs}}}}
	}
	ctx := actor.WithActor(context.Background(), &actor.Actor{UserID: "u-1"})
	if err := c.Role(ctx, edit([]string{"keys"}, []string{"get"})); err != nil {
		t.Fatalf("edit within what the editor holds: %v", err)
	}
	if err := c.Role(ctx, edit([]string{"*"}, []string{"*"})); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("widening a bound role = %v, want forbidden", err)
	}
}

// Catalog-owned rate limits and policies carry no credentials, so any scope
// may draw on them; another tenant's rows and host keys stay off limits.
func TestCatalogOwnedLimitsAndPoliciesReferenceableFromAnyScope(t *testing.T) {
	owner := func(kind meta.OwnerKind, id string) meta.Owner { return meta.Owner{Kind: kind, ID: id} }
	rls := map[string]*ratelimit.RateLimit{}
	pols := map[string]*policy.Policy{}
	for id, o := range map[string]meta.Owner{
		"system":        owner(meta.OwnerSystem, ""),
		"provider":      owner(meta.OwnerProvider, "pv-1"),
		"host":          owner(meta.OwnerHost, "h-1"),
		"same-project":  owner(meta.OwnerProject, "p-1"),
		"other-project": owner(meta.OwnerProject, "p-2"),
		"team":          owner(meta.OwnerTeam, "t-1"),
	} {
		rls[id] = &ratelimit.RateLimit{Meta: meta.Metadata{ID: id, Name: id, Owner: o}}
		pols[id] = &policy.Policy{Meta: meta.Metadata{ID: id, Name: id, Owner: o}}
	}
	c := Checker{Authz: scopingAuthz{}, Rows: Lookup{
		RateLimit: func(_ context.Context, id string) *ratelimit.RateLimit { return rls[id] },
		Policy:    func(_ context.Context, id string) *policy.Policy { return pols[id] },
	}}
	ctx := actor.WithActor(context.Background(), &actor.Actor{UserID: "u-1"})
	project := owner(meta.OwnerProject, "p-1")
	scopes := map[string]meta.Owner{"user": owner(meta.OwnerUser, "u-1"), "team": owner(meta.OwnerTeam, "t-1"), "project": project}

	for _, target := range []string{"system", "provider", "host"} {
		for name, ref := range scopes {
			if err := c.RateLimitRef(ctx, target, ref); err != nil {
				t.Errorf("%s policy -> %s-owned rate limit: %v", name, target, err)
			}
		}
	}
	for _, target := range []string{"system", "provider"} {
		for name, ref := range scopes {
			if err := c.PolicyRef(ctx, target, ref); err != nil {
				t.Errorf("%s row -> %s-owned policy: %v", name, target, err)
			}
		}
	}
	for _, tc := range []struct {
		target string
		ok     bool
	}{
		{"same-project", true},
		{"other-project", false},
		{"team", false},
	} {
		if err := c.RateLimitRef(ctx, tc.target, project); (err == nil) != tc.ok {
			t.Errorf("project policy -> %s rate limit: err=%v, want ok=%v", tc.target, err, tc.ok)
		}
	}
	for name, ref := range scopes {
		if err := c.PolicyRef(ctx, "host", ref); err == nil {
			t.Errorf("%s row bound a host tier policy", name)
		}
	}
}

// Host keys are credentials: the catalog allowance does not reach them.
func TestProjectPolicyHostKeyScope(t *testing.T) {
	keys := map[string]*hostkey.HostKey{
		"own":      {Meta: meta.Metadata{ID: "own", Name: "own", Owner: meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}}},
		"other":    {Meta: meta.Metadata{ID: "other", Name: "other", Owner: meta.Owner{Kind: meta.OwnerProject, ID: "p-2"}}},
		"personal": {Meta: meta.Metadata{ID: "personal", Name: "personal", Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-2"}}},
	}
	c := Checker{Authz: scopingAuthz{}, Rows: Lookup{HostKey: func(_ context.Context, id string) *hostkey.HostKey { return keys[id] }}}
	ctx := actor.WithActor(context.Background(), &actor.Actor{UserID: "u-1"})
	project := meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}
	for _, tc := range []struct {
		key string
		ok  bool
	}{
		{"own", true},
		{"other", false},
		{"personal", false},
	} {
		if err := c.HostKeyRefs(ctx, []string{tc.key}, project); (err == nil) != tc.ok {
			t.Errorf("project policy -> %s host key: err=%v, want ok=%v", tc.key, err, tc.ok)
		}
	}
}
