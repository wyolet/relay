package refcheck

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
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

func hostLookup(hosts ...*host.Host) func(context.Context, string) *host.Host {
	return func(_ context.Context, id string) *host.Host {
		for _, h := range hosts {
			if h.Meta.ID == id {
				return h
			}
		}
		return nil
	}
}

// A binding owned by anyone but a personal host's owner would carry that
// host into the binder's scope.
func TestBindingOnAPersonalHostMustShareItsOwner(t *testing.T) {
	bob := meta.Owner{Kind: meta.OwnerUser, ID: "u-bob"}
	personal := &host.Host{Meta: meta.Metadata{ID: "h-bob", Name: "bob-box", Owner: bob}}
	shared := &host.Host{Meta: meta.Metadata{ID: "h-sys", Name: "sys", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	c := Checker{Rows: Lookup{Host: hostLookup(personal, shared)}}
	bind := func(hostID string, owner meta.Owner) *binding.Binding {
		return &binding.Binding{Meta: meta.Metadata{Name: "b", Owner: owner}, Spec: binding.Spec{HostID: hostID}}
	}
	ctx := context.Background()
	if err := c.HostBinding(ctx, bind("h-bob", bob)); err != nil {
		t.Fatalf("owner binding their own host: %v", err)
	}
	for _, other := range []meta.Owner{{Kind: meta.OwnerUser, ID: "u-alice"}, {Kind: meta.OwnerSystem}, {Kind: meta.OwnerUser}} {
		if err := c.HostBinding(ctx, bind("h-bob", other)); err == nil {
			t.Fatalf("binding owned by %+v accepted on bob's personal host", other)
		}
	}
	if err := c.HostBinding(ctx, bind("h-sys", meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"})); err != nil {
		t.Fatalf("binding on a shared host: %v", err)
	}
}
