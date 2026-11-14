package control

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/meta"
)

// Caller accounts are relay's: none is created through the API, and only a closed member account is deleted on its own; a provider account is the caller's to create and delete, a shared one is made by sharing.
func TestGuardAccount_CallerAccountsAreRelays(t *testing.T) {
	ctx := context.Background()
	guard := guardAccount(Deps{})
	team := account.ForTeam(meta.NewID(), "acme", "")
	member := account.Member(meta.NewID(), "alice", meta.Owner{Kind: meta.OwnerProject, ID: meta.NewID()}, "ml", "")
	closed := *member
	closed.Spec.Closed = true
	provider := &account.Account{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme-openai", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: account.Spec{HostID: meta.NewID()}}
	share := account.Share(provider, account.Receiver{Kind: account.ReceiverTeam, ID: team.Meta.Owner.ID}, team.Meta.Owner, "acme")

	for _, tc := range []struct {
		name, action       string
		existing, incoming *account.Account
		status             int
		msg                string
	}{
		{"creating a team account", "create", nil, team, http.StatusBadRequest, "spec.hostId is required: team, project, personal, member and service accounts are made by relay with their team, project, user, membership or service account"},
		{"creating a shared account", "create", nil, share, http.StatusBadRequest, "spec.sourceAccountId is set by sharing: POST /accounts/{ref}/shares shares the real account"},
		{"deleting a team account", "delete", team, nil, http.StatusConflict, `account "team-acme" is a team account: it goes with its team; only a closed member account is deleted on its own`},
		{"deleting an open member account", "delete", member, nil, http.StatusConflict, `account "alice-project-ml" is a member account: it goes with its membership; only a closed member account is deleted on its own`},
		{"deleting a closed member account", "delete", &closed, nil, 0, ""},
		{"deleting a provider account", "delete", provider, nil, 0, ""},
		{"deleting a shared account", "delete", share, nil, 0, ""},
	} {
		err := guard(ctx, tc.action, tc.existing, tc.incoming)
		if got := statusOf(t, err); got != tc.status || (err != nil && err.Error() != tc.msg) {
			t.Errorf("%s: %d %v, want %d %s", tc.name, got, err, tc.status, tc.msg)
		}
	}
	var ae *httpapi.APIError
	if err := guard(ctx, "delete", team, nil); !errors.As(err, &ae) || ae.Err.Code != codeAccountManaged {
		t.Errorf("deleting a team account = %#v, want code %s", err, codeAccountManaged)
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

// A PUT keeps what relay owns on an account whatever the body says: the kind, holder, closed mark and share source; a caller account also its owner and no host, a shared account its owner and host. A provider account's owner and host are the body's.
func TestMergeAccountServerFields(t *testing.T) {
	alice, proj, hStored, hBody, realID, otherID, mallory, sharerID := meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID()
	scope, bodyOwner := meta.Owner{Kind: meta.OwnerProject, ID: proj}, meta.Owner{Kind: meta.OwnerSystem}
	member := account.Member(alice, "alice", scope, "ml", "")
	member.Spec.Closed, member.FollowsRoleBindings = true, true
	provider := &account.Account{Meta: meta.Metadata{Name: "acme-openai", Owner: scope}, Spec: account.Spec{HostID: hStored, Kind: account.KindProvider}}
	share := account.Share(&account.Account{Meta: meta.Metadata{ID: realID, Name: "real"}, Spec: account.Spec{HostID: hStored}}, account.Receiver{Kind: account.ReceiverProject, ID: proj}, scope, "ml")
	sharer := &account.ShareParty{ID: sharerID}
	share.Spec.SharedBy = sharer
	body := func() *account.Account {
		return &account.Account{Meta: meta.Metadata{Name: "renamed", Owner: bodyOwner}, Spec: account.Spec{
			Kind: account.KindTeam, HostID: hBody, Holder: &account.Holder{Kind: account.HolderUser, ID: mallory},
			PayloadLogging: account.PayloadLoggingOn, SourceAccountID: otherID, SharedBy: &account.ShareParty{ID: mallory},
		}}
	}
	want := func(owner meta.Owner, spec account.Spec, follows bool) *account.Account {
		spec.PayloadLogging = account.PayloadLoggingOn
		return &account.Account{Meta: meta.Metadata{Name: "renamed", Owner: owner}, Spec: spec, FollowsRoleBindings: follows}
	}
	for _, tc := range []struct {
		name   string
		stored *account.Account
		want   *account.Account
	}{
		{"a member account keeps its place", member, want(scope, account.Spec{Kind: account.KindMember, Holder: member.Spec.Holder, Closed: true}, true)},
		{"a provider account takes the body's owner and host", provider, want(bodyOwner, account.Spec{Kind: account.KindProvider, HostID: hBody}, false)},
		{"a shared account follows its share", share, want(scope, account.Spec{Kind: account.KindProvider, HostID: hStored, SourceAccountID: realID, SharedBy: sharer}, false)},
	} {
		got := body()
		mergeAccountServerFields(tc.stored, got)
		if diff := cmp.Diff(tc.want, got); diff != "" {
			t.Errorf("%s (-want +got):\n%s", tc.name, diff)
		}
		if err := got.Validate(); err != nil {
			t.Errorf("%s: the merged account does not validate: %v", tc.name, err)
		}
	}
}
