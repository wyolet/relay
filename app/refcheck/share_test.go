package refcheck

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/pkg/meta"
)

// editsOnly lets the caller edit accounts of the listed owners and see every row but the hidden ids; every other action passes.
type editsOnly struct {
	owners []meta.Owner
	hidden []string
}

func (e editsOnly) Authorize(_ context.Context, action string, r authz.Resource) error {
	if action != "accounts.update" {
		return nil
	}
	for _, o := range e.owners {
		if r.Owner != nil && *r.Owner == o {
			return nil
		}
	}
	return authz.ErrForbidden
}

func (e editsOnly) Visible(_ context.Context, _, id string, _ meta.Owner) bool {
	return !slices.Contains(e.hidden, id)
}

func as(userID string) context.Context {
	return actor.WithActor(context.Background(), &actor.Actor{UserID: userID})
}

func code(err error) string {
	var re *Error
	if errors.As(err, &re) {
		return re.Code
	}
	return ""
}

var (
	teamOne    = meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"}
	teamTwo    = meta.Owner{Kind: meta.OwnerTeam, ID: "t-2"}
	projectOne = meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}
	alice      = meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}
)

// shareRows adds alice's personal provider account, a share of team-1's account with team-2, and service account sa-1 in project p-1.
func shareRows() *linkRows {
	r := newLinkRows()
	r.accounts["acct-alice"] = &account.Account{Meta: meta.Metadata{ID: "acct-alice", Name: "acct-alice", Owner: alice}, Spec: account.Spec{HostID: "h-1"}}
	r.accounts["share-t2"] = account.Share(r.accounts["acct-team-1"], account.Receiver{Kind: account.ReceiverTeam, ID: "t-2"}, teamTwo, "t-2")
	r.accounts["share-t2"].Meta.ID = "share-t2"
	return r
}

func (r *linkRows) shareChecker(a authz.Authorizer) Checker {
	c := r.checker(a)
	c.Rows.ServiceAccount = func(_ context.Context, id string) *serviceaccount.ServiceAccount {
		if id != "sa-1" {
			return nil
		}
		return &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: "sa-1", Name: "sa-1", Owner: projectOne}, Spec: serviceaccount.Spec{ProjectID: "p-1"}}
	}
	c.Rows.MissingUsers = func(_ context.Context, ids []string) ([]string, error) {
		var out []string
		for _, id := range ids {
			if id != "u-alice" && id != "u-bob" && id != "u-hidden" {
				out = append(out, id)
			}
		}
		return out, nil
	}
	c.Rows.UserVisible = func(_ context.Context, id string) bool { return id != "u-hidden" }
	return c
}

// Only who may edit the real account shares it: its user for a personal account, an admin of its team (or the system) otherwise. A share is never shared on, and a receiver must exist.
func TestShareRules(t *testing.T) {
	rows := shareRows()
	bob := account.Receiver{Kind: account.ReceiverUser, ID: "u-bob"}
	toBob := meta.Owner{Kind: meta.OwnerUser, ID: "u-bob"}
	admin := actor.WithActor(context.Background(), &actor.Actor{AdminToken: true})
	everything := editsOnly{owners: []meta.Owner{alice, teamOne, teamTwo, projectOne, {Kind: meta.OwnerSystem}}}
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		caller authz.Authorizer
		src    string
		to     account.Receiver
		owner  meta.Owner
		status int
		code   string
	}{
		{"its user shares a personal account", as("u-alice"), editsOnly{}, "acct-alice", bob, toBob, 0, ""},
		{"an admin does not share a personal account", admin, everything, "acct-alice", bob, toBob, http.StatusForbidden, ""},
		{"a user who may edit every account does not share another's personal one", as("u-carol"), everything, "acct-alice", bob, toBob, http.StatusForbidden, ""},
		{"team admin shares the team's account", as("u-dave"), editsOnly{owners: []meta.Owner{teamOne}}, "acct-team-1", account.Receiver{Kind: account.ReceiverProject, ID: "p-2"}, meta.Owner{Kind: meta.OwnerProject, ID: "p-2"}, 0, ""},
		{"a team member who may not edit its accounts", as("u-erin"), editsOnly{}, "acct-team-1", account.Receiver{Kind: account.ReceiverTeam, ID: "t-2"}, teamTwo, http.StatusForbidden, ""},
		{"the receiving admin cannot share on", as("u-bob"), editsOnly{owners: []meta.Owner{teamTwo}}, "share-t2", bob, toBob, http.StatusConflict, CodeReshare},
		{"a missing user", as("u-alice"), editsOnly{}, "acct-alice", account.Receiver{Kind: account.ReceiverUser, ID: "u-gone"}, meta.Owner{Kind: meta.OwnerUser, ID: "u-gone"}, http.StatusNotFound, ""},
		{"a user the sharer may not see", as("u-alice"), editsOnly{}, "acct-alice", account.Receiver{Kind: account.ReceiverUser, ID: "u-hidden"}, meta.Owner{Kind: meta.OwnerUser, ID: "u-hidden"}, http.StatusNotFound, ""},
		{"a missing team", as("u-alice"), editsOnly{}, "acct-alice", account.Receiver{Kind: account.ReceiverTeam, ID: "t-9"}, meta.Owner{Kind: meta.OwnerTeam, ID: "t-9"}, http.StatusNotFound, ""},
		{"a team the sharer may not see", as("u-alice"), editsOnly{hidden: []string{"t-2"}}, "acct-alice", account.Receiver{Kind: account.ReceiverTeam, ID: "t-2"}, teamTwo, http.StatusNotFound, ""},
		{"a service account's share is its project's", as("u-alice"), editsOnly{}, "acct-alice", account.Receiver{Kind: account.ReceiverServiceAccount, ID: "sa-1"}, projectOne, 0, ""},
		{"a service account's share in another project", as("u-alice"), editsOnly{}, "acct-alice", account.Receiver{Kind: account.ReceiverServiceAccount, ID: "sa-1"}, meta.Owner{Kind: meta.OwnerProject, ID: "p-2"}, http.StatusBadRequest, ""},
		{"sharing with its own owner", as("u-alice"), editsOnly{}, "acct-alice", account.Receiver{Kind: account.ReceiverUser, ID: "u-alice"}, alice, http.StatusConflict, CodeAlreadyReachable},
		{"a team's account with a project of the team", as("u-dave"), editsOnly{owners: []meta.Owner{teamOne}}, "acct-team-1", account.Receiver{Kind: account.ReceiverProject, ID: "p-1"}, projectOne, http.StatusConflict, CodeAlreadyReachable},
		{"a system account, which every scope reaches", admin, everything, "acct-shared", account.Receiver{Kind: account.ReceiverTeam, ID: "t-2"}, teamTwo, http.StatusConflict, CodeAlreadyReachable},
	} {
		src := rows.accounts[tc.src]
		share := account.Share(src, tc.to, tc.owner, "x")
		share.Meta.ID = meta.NewID()
		err := rows.shareChecker(tc.caller).Share(tc.ctx, src, share)
		if got := status(err); got != tc.status || code(err) != tc.code {
			t.Errorf("%s: status %d code %q (%v), want %d %q", tc.name, got, code(err), err, tc.status, tc.code)
		}
	}
}

// A share holds no keys: a host key naming one is refused with account_shared, pointing at the real account.
func TestHostKeyOnShareRefused(t *testing.T) {
	rows := shareRows()
	k := &hostkey.HostKey{Meta: meta.Metadata{Name: "k", Owner: teamTwo}, Spec: hostkey.Spec{HostID: "h-1", AccountID: "share-t2"}}
	err := rows.shareChecker(nil).HostKey(context.Background(), k)
	if status(err) != http.StatusConflict || code(err) != CodeAccountShared {
		t.Fatalf("host key on a share: %v, want 409 %s", err, CodeAccountShared)
	}
}

// A pool draws on accounts at its host: its scope's own and the ones its scope may use, and a share only in the scope it was shared with.
func TestPoolAccounts(t *testing.T) {
	rows := shareRows()
	rows.accounts["acct-h2"] = &account.Account{Meta: meta.Metadata{ID: "acct-h2", Name: "acct-h2", Owner: teamTwo}, Spec: account.Spec{HostID: "h-2"}}
	c := rows.shareChecker(nil)
	mk := func(owner meta.Owner, accounts ...string) *pool.Pool {
		return &pool.Pool{Meta: meta.Metadata{Name: "p", Owner: owner}, Spec: pool.Spec{HostID: "h-1", AccountIDs: accounts}}
	}
	for _, tc := range []struct {
		name string
		p    *pool.Pool
		want int
	}{
		{"its own and the system's accounts", mk(teamTwo, "acct-team-2", "acct-shared"), 0},
		{"a share in its own scope", mk(teamTwo, "share-t2"), 0},
		{"a team's account from its project", mk(projectOne, "acct-team-1"), 0},
		{"a share from another scope", mk(teamOne, "share-t2"), http.StatusBadRequest},
		{"another team's account", mk(teamOne, "acct-team-2"), http.StatusBadRequest},
		{"an account at another host", mk(teamTwo, "acct-h2"), http.StatusBadRequest},
		{"an account that does not exist", mk(teamTwo, "acct-gone"), http.StatusBadRequest},
	} {
		if got := status(c.Pool(context.Background(), tc.p)); got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
}
