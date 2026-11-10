package control

import (
	"context"
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/meta"
)

// Caller accounts are relay's: none is created through the API, and only a closed member account is deleted on its own; a provider account is the caller's to create and delete.
func TestGuardAccount_CallerAccountsAreRelays(t *testing.T) {
	ctx := context.Background()
	guard := guardAccount(Deps{})
	team := account.ForTeam(meta.NewID(), "acme", "")
	member := account.Member(meta.NewID(), "alice", meta.Owner{Kind: meta.OwnerProject, ID: meta.NewID()}, "ml", "")
	closed := *member
	closed.Spec.Closed = true
	provider := &account.Account{Meta: meta.Metadata{Name: "acme-openai", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: account.Spec{HostID: meta.NewID()}}

	if got := statusOf(t, guard(ctx, "create", nil, team)); got != http.StatusBadRequest {
		t.Errorf("creating a team account: status %d, want 400", got)
	}
	for _, tc := range []struct {
		name string
		a    *account.Account
		want int
	}{
		{"a team account", team, http.StatusConflict},
		{"an open member account", member, http.StatusConflict},
		{"a closed member account", &closed, 0},
		{"a provider account", provider, 0},
	} {
		if got := statusOf(t, guard(ctx, "delete", tc.a, nil)); got != tc.want {
			t.Errorf("deleting %s: status %d, want %d", tc.name, got, tc.want)
		}
	}
}

// payloadLogging decides whether a user's request bodies are kept, so the user it constrains does not set it: a personal account's is an admin's, a member account's its scope admin's but never its holder's.
func TestGuardAccount_PayloadLoggingIsNotTheHoldersToSet(t *testing.T) {
	guard := guardAccount(Deps{Authz: scopedAllow{}})
	as := func(userID string, roles ...string) context.Context {
		return actor.WithActor(context.Background(), &actor.Actor{UserID: userID, Roles: roles})
	}
	scope := meta.Owner{Kind: meta.OwnerProject, ID: meta.NewID()}
	personal := account.Personal("u-alice", "alice")
	member := account.Member("u-alice", "alice", scope, "ml", "")
	project := account.ForProject(scope.ID, "ml", "")
	on := func(a *account.Account) *account.Account {
		b := *a
		b.Spec.PayloadLogging = account.PayloadLoggingOn
		return &b
	}
	for _, tc := range []struct {
		name string
		ctx  context.Context
		a    *account.Account
		want int
	}{
		{"the user on their personal account", as("u-alice"), personal, http.StatusForbidden},
		{"an admin on a personal account", as("u-root", user.RoleAdmin), personal, 0},
		{"the holder on their member account", as("u-alice"), member, http.StatusForbidden},
		{"a scope admin on someone's member account", as("u-bob"), member, 0},
		{"a scope admin on the project's account", as("u-bob"), project, 0},
	} {
		if got := statusOf(t, guard(tc.ctx, "update", tc.a, on(tc.a))); got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
	if err := guard(as("u-alice"), "update", personal, personal); err != nil {
		t.Errorf("an update leaving payloadLogging alone: %v", err)
	}
}

// A PUT keeps what fixes a caller account's place: its holder, owner, kind and closed mark, whatever the body says; a provider account keeps only the server-managed fields.
func TestMergeAccountServerFields(t *testing.T) {
	stored := account.Member(meta.NewID(), "alice", meta.Owner{Kind: meta.OwnerProject, ID: meta.NewID()}, "ml", "")
	stored.Spec.Closed, stored.FollowsRoleBindings = true, true
	body := &account.Account{Meta: meta.Metadata{Name: "renamed", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: account.Spec{HostID: meta.NewID(), PayloadLogging: account.PayloadLoggingOn}}
	mergeAccountServerFields(stored, body)
	if body.Meta.Owner != stored.Meta.Owner || body.Spec.Holder != stored.Spec.Holder || body.Spec.HostID != "" || !body.Spec.Closed || !body.FollowsRoleBindings {
		t.Fatalf("merged = %+v / %+v, want the stored place kept", body.Meta.Owner, body.Spec)
	}
	if body.Meta.Name != "renamed" || body.Spec.PayloadLogging != account.PayloadLoggingOn {
		t.Fatalf("merged = %+v, want the editable fields from the body", body)
	}
	if err := body.Validate(); err != nil {
		t.Fatalf("merged member account: %v", err)
	}
}
