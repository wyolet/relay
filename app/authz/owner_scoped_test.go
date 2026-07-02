package authz

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/user"
)

func actorCtx(a *actor.Actor) context.Context {
	if a == nil {
		return context.Background()
	}
	return actor.WithActor(context.Background(), a)
}

var (
	alice = &actor.Actor{UserID: "u-alice", Username: "alice"}
	bob   = &actor.Actor{UserID: "u-bob", Username: "bob"}
	root  = &actor.Actor{UserID: "u-root", Username: "root", Roles: []string{user.RoleAdmin}}
	token = &actor.Actor{AdminToken: true}

	systemOwner   = meta.Owner{Kind: meta.OwnerSystem}
	providerOwner = meta.Owner{Kind: meta.OwnerProvider, ID: "p-1"}
	aliceOwner    = meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}
	operatorOwner = meta.Owner{Kind: meta.OwnerUser} // empty id = operator row
)

func TestOwnerScopedAuthorize(t *testing.T) {
	s := OwnerScoped{}
	tests := []struct {
		name   string
		who    *actor.Actor
		action string
		res    Resource
		want   error
	}{
		{"unauthenticated denied", nil, "policies.list", Resource{Kind: "policy"}, ErrUnauthenticated},

		// Reads on catalog kinds: any authenticated user (rows gated by Visible).
		{"user lists policies", alice, "policies.list", Resource{Kind: "policy"}, nil},
		{"user reads model", alice, "models.read", Resource{Kind: "model", Name: "gpt-4o"}, nil},
		{"user reads model overlay", alice, "models.overlay.read", Resource{Kind: "model", ID: "m-1"}, nil},

		// Usage/logs reads pass authz; their handlers scope rows to the
		// caller's relay-keys. The read_all probe stays admin-only.
		{"user reads usage (handler-scoped)", alice, "usage.read", Resource{Kind: "usage"}, nil},
		{"user lists logs (handler-scoped)", alice, "logs.list", Resource{Kind: "logs"}, nil},
		{"user denied usage read_all probe", alice, "usage.read_all", Resource{Kind: "usage"}, ErrForbidden},
		{"admin passes usage read_all probe", root, "usage.read_all", Resource{Kind: "usage"}, nil},

		// Reads on non-scoped kinds: admin only.
		{"user denied settings read", alice, "settings.read", Resource{Kind: "settings"}, ErrForbidden},
		{"user denied debug", alice, "debug.snapshot", Resource{Kind: "debug"}, ErrForbidden},
		{"admin role reads settings", root, "settings.read", Resource{Kind: "settings"}, nil},
		{"admin token reads usage", token, "usage.read", Resource{Kind: "usage"}, nil},

		// Mutations with no owner info fail closed for non-admins.
		{"user denied ownerless update", alice, "policies.update", Resource{Kind: "policy", ID: "x"}, ErrForbidden},
		{"user denied reload", alice, "system.reload", Resource{Kind: "system"}, ErrForbidden},
		{"user denied master-key rotate", alice, "master-key.rotate", Resource{Kind: "master-key"}, ErrForbidden},
		{"user denied overlay update", alice, "models.overlay.update", Resource{Kind: "model", ID: "m-1"}, ErrForbidden},
		{"admin role allowed ownerless update", root, "policies.update", Resource{Kind: "policy", ID: "x"}, nil},
		{"admin token allowed reload", token, "system.reload", Resource{Kind: "system"}, nil},

		// Mutations on owned rows.
		{"owner updates own row", alice, "policies.update", Resource{Kind: "policy", ID: "x", Owner: &aliceOwner}, nil},
		{"owner deletes own row", alice, "policies.delete", Resource{Kind: "policy", ID: "x", Owner: &aliceOwner}, nil},
		{"owner creates user-owned row", alice, "policies.create", Resource{Kind: "policy", Owner: &aliceOwner}, nil},
		{"other user denied", bob, "policies.update", Resource{Kind: "policy", ID: "x", Owner: &aliceOwner}, ErrForbidden},
		{"user denied catalog row update", alice, "models.update", Resource{Kind: "model", ID: "x", Owner: &providerOwner}, ErrForbidden},
		{"user denied system row delete", alice, "providers.delete", Resource{Kind: "provider", ID: "x", Owner: &systemOwner}, ErrForbidden},
		{"user denied operator row update", alice, "relay-keys.update", Resource{Kind: "relay-key", ID: "x", Owner: &operatorOwner}, ErrForbidden},
		{"admin role updates any row", root, "policies.update", Resource{Kind: "policy", ID: "x", Owner: &aliceOwner}, nil},
		{"admin token updates operator row", token, "relay-keys.update", Resource{Kind: "relay-key", ID: "x", Owner: &operatorOwner}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := s.Authorize(actorCtx(tt.who), tt.action, tt.res)
			if got != tt.want {
				t.Fatalf("Authorize(%q) = %v, want %v", tt.action, got, tt.want)
			}
		})
	}
}

func TestOwnerScopedVisible(t *testing.T) {
	s := OwnerScoped{}
	tests := []struct {
		name  string
		who   *actor.Actor
		owner meta.Owner
		want  bool
	}{
		{"unauthenticated sees nothing", nil, systemOwner, false},
		{"catalog rows visible to all", alice, systemOwner, true},
		{"provider rows visible to all", alice, providerOwner, true},
		{"own rows visible", alice, aliceOwner, true},
		{"other user's rows hidden", bob, aliceOwner, false},
		{"operator rows hidden from users", alice, operatorOwner, false},
		{"admin role sees everything", root, aliceOwner, true},
		{"admin role sees operator rows", root, operatorOwner, true},
		{"admin token sees everything", token, aliceOwner, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := s.Visible(actorCtx(tt.who), "policy", tt.owner); got != tt.want {
				t.Fatalf("Visible(%+v) = %v, want %v", tt.owner, got, tt.want)
			}
		})
	}
}

// AlwaysAllowAuthenticated must not implement Scoper — single-user reads
// stay unfiltered, byte-identical to pre-scoping behavior.
func TestAlwaysAllowIsNotScoper(t *testing.T) {
	var a Authorizer = AlwaysAllowAuthenticated{}
	if _, ok := a.(Scoper); ok {
		t.Fatal("AlwaysAllowAuthenticated must not implement Scoper")
	}
}
