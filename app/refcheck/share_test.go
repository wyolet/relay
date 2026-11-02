package refcheck

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/serviceaccount"
)

// editsOnly lets the caller edit accounts of the listed owners; every other action passes.
type editsOnly struct{ owners []meta.Owner }

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

func (editsOnly) Visible(context.Context, string, string, meta.Owner) bool { return true }

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
			if id != "u-alice" && id != "u-bob" {
				out = append(out, id)
			}
		}
		return out, nil
	}
	return c
}

// Only who may edit the real account shares it: its user for a personal account, an admin of its team (or the system) otherwise. A share is never shared on, and a receiver must exist.
func TestShareRules(t *testing.T) {
	rows := shareRows()
	ctx := context.Background()
	bob := account.Receiver{Kind: account.ReceiverUser, ID: "u-bob"}
	for _, tc := range []struct {
		name   string
		caller authz.Authorizer
		src    string
		to     account.Receiver
		owner  meta.Owner
		status int
		code   string
	}{
		{"owner shares a personal account", editsOnly{[]meta.Owner{alice}}, "acct-alice", bob, meta.Owner{Kind: meta.OwnerUser, ID: "u-bob"}, 0, ""},
		{"team admin shares the team's account", editsOnly{[]meta.Owner{teamOne}}, "acct-team-1", account.Receiver{Kind: account.ReceiverProject, ID: "p-2"}, meta.Owner{Kind: meta.OwnerProject, ID: "p-2"}, 0, ""},
		{"a stranger is refused", editsOnly{[]meta.Owner{teamTwo}}, "acct-alice", bob, meta.Owner{Kind: meta.OwnerUser, ID: "u-bob"}, http.StatusForbidden, ""},
		{"the receiving admin cannot share on", editsOnly{[]meta.Owner{teamTwo}}, "share-t2", bob, meta.Owner{Kind: meta.OwnerUser, ID: "u-bob"}, http.StatusConflict, CodeReshare},
		{"a missing user", editsOnly{[]meta.Owner{alice}}, "acct-alice", account.Receiver{Kind: account.ReceiverUser, ID: "u-gone"}, meta.Owner{Kind: meta.OwnerUser, ID: "u-gone"}, http.StatusNotFound, ""},
		{"a missing team", editsOnly{[]meta.Owner{alice}}, "acct-alice", account.Receiver{Kind: account.ReceiverTeam, ID: "t-9"}, meta.Owner{Kind: meta.OwnerTeam, ID: "t-9"}, http.StatusNotFound, ""},
		{"a service account's share is its project's", editsOnly{[]meta.Owner{alice}}, "acct-alice", account.Receiver{Kind: account.ReceiverServiceAccount, ID: "sa-1"}, projectOne, 0, ""},
		{"a service account's share in another project", editsOnly{[]meta.Owner{alice}}, "acct-alice", account.Receiver{Kind: account.ReceiverServiceAccount, ID: "sa-1"}, meta.Owner{Kind: meta.OwnerProject, ID: "p-2"}, http.StatusBadRequest, ""},
		{"sharing with its own owner", editsOnly{[]meta.Owner{alice}}, "acct-alice", account.Receiver{Kind: account.ReceiverUser, ID: "u-alice"}, alice, http.StatusBadRequest, ""},
	} {
		src := rows.accounts[tc.src]
		share := account.Share(src, tc.to, tc.owner, "x")
		share.Meta.ID = meta.NewID()
		err := rows.shareChecker(tc.caller).Share(ctx, src, share)
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
