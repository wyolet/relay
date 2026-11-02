package control

import (
	"context"
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/meta"
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
