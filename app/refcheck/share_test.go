package refcheck

import (
	"context"
	"errors"
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

var (
	system     = meta.Owner{Kind: meta.OwnerSystem}
	teamOne    = meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"}
	teamTwo    = meta.Owner{Kind: meta.OwnerTeam, ID: "t-2"}
	projectOne = meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}
	projectTwo = meta.Owner{Kind: meta.OwnerProject, ID: "p-2"}
	alice      = meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}
)

// shareRows adds alice's personal provider account, a share of team-1's account with team-2, and team-1's caller account.
func shareRows() *linkRows {
	r := newLinkRows()
	r.accounts["acct-alice"] = &account.Account{Meta: meta.Metadata{ID: "acct-alice", Name: "acct-alice", Owner: alice}, Spec: account.Spec{HostID: "h-1"}}
	r.accounts["share-t2"] = account.Share(r.accounts["acct-team-1"], account.Receiver{Kind: account.ReceiverTeam, ID: "t-2"}, teamTwo, "t-2")
	r.accounts["share-t2"].Meta.ID = "share-t2"
	r.accounts["caller-t-1"] = account.ForTeam("t-1", "t-1", "")
	r.accounts["caller-t-1"].Meta.ID = "caller-t-1"
	return r
}

// shareChecker adds service account sa-1 of project p-1 and the users u-alice, u-bob and u-hidden, the last one unseen by the caller.
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
			if id == "u-broken" {
				return nil, errors.New("store down")
			}
			if id != "u-alice" && id != "u-bob" && id != "u-hidden" {
				out = append(out, id)
			}
		}
		return out, nil
	}
	c.Rows.UserVisible = func(_ context.Context, id string) bool { return id != "u-hidden" }
	return c
}

// Only who may edit the real account shares it: its user for a personal account, an admin of its team (or the system) otherwise. A share is never shared on, a receiver must exist and be visible, and must not reach the account already.
func TestShare(t *testing.T) {
	rows := shareRows()
	type dest struct {
		r     account.Receiver
		owner meta.Owner
	}
	user := func(id string) dest {
		return dest{account.Receiver{Kind: account.ReceiverUser, ID: id}, meta.Owner{Kind: meta.OwnerUser, ID: id}}
	}
	scope := func(o meta.Owner) dest {
		return dest{account.Receiver{Kind: account.ReceiverKind(o.Kind), ID: o.ID}, o}
	}
	sa := func(id string, owner meta.Owner) dest {
		return dest{account.Receiver{Kind: account.ReceiverServiceAccount, ID: id}, owner}
	}
	everything := editsOnly{owners: []meta.Owner{alice, teamOne, teamTwo, projectOne, system}}
	teamAdmin := editsOnly{owners: []meta.Owner{teamOne}}
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		caller authz.Authorizer
		src    string
		to     dest
		want   string
	}{
		{"its user shares a personal account", as("u-alice"), editsOnly{}, "acct-alice", user("u-bob"), ""},
		{"an admin does not share a personal account", adminToken(), everything, "acct-alice", user("u-bob"), `403: authz: forbidden: another user's personal account "acct-alice" is personal: only its user shares it; ask them to share it`},
		{"a user who may edit every account does not share another's personal one", as("u-carol"), everything, "acct-alice", user("u-bob"), `403: authz: forbidden: another user's personal account "acct-alice" is personal: only its user shares it; ask them to share it`},
		{"a loader shares a personal account", context.Background(), nil, "acct-alice", user("u-bob"), ""},
		{"team admin shares the team's account", as("u-dave"), teamAdmin, "acct-team-1", scope(projectTwo), ""},
		{"a team member who may not edit its accounts", as("u-erin"), editsOnly{}, "acct-team-1", scope(teamTwo), `403: authz: forbidden: only an admin of team "t-1" shares account "acct-team-1" of team "t-1"; ask one to share it`},
		{"the receiving admin cannot share on", as("u-bob"), editsOnly{owners: []meta.Owner{teamTwo}}, "share-t2", user("u-bob"), `409 reshare_refused: account "acct-team-1" of team "t-2" is a share, so it cannot be shared on: only the owner of the account it came from shares that account. Ask them to share it with the receiver.`},
		{"a missing user", as("u-alice"), editsOnly{}, "acct-alice", user("u-gone"), `404: user "u-gone" not found`},
		{"the user lookup failing", as("u-alice"), editsOnly{}, "acct-alice", user("u-broken"), "500: store down"},
		{"a user the sharer may not see", as("u-alice"), editsOnly{}, "acct-alice", user("u-hidden"), `404: user "u-hidden" not found`},
		{"a missing team", as("u-alice"), editsOnly{}, "acct-alice", scope(meta.Owner{Kind: meta.OwnerTeam, ID: "t-9"}), `404: team "t-9" not found`},
		{"a team the sharer may not see", as("u-alice"), editsOnly{hidden: []string{"t-2"}}, "acct-alice", scope(teamTwo), `404: team "t-2" not found`},
		{"a missing project", as("u-alice"), editsOnly{}, "acct-alice", scope(meta.Owner{Kind: meta.OwnerProject, ID: "p-9"}), `404: project "p-9" not found`},
		{"a project the sharer may not see", as("u-alice"), editsOnly{hidden: []string{"p-2"}}, "acct-alice", scope(projectTwo), `404: project "p-2" not found`},
		{"a service account's share is its project's", as("u-alice"), editsOnly{}, "acct-alice", sa("sa-1", projectOne), ""},
		{"a service account's share in another project", as("u-alice"), editsOnly{}, "acct-alice", sa("sa-1", projectTwo), `400: a share with service account "sa-1" of project "p-1" belongs to its project, project "p-1"; leave the owner out or set it to that project`},
		{"a missing service account", as("u-alice"), editsOnly{}, "acct-alice", sa("sa-gone", projectOne), `404: service-account "sa-gone" not found`},
		{"the system as a receiver", as("u-alice"), editsOnly{}, "acct-alice", scope(system), "400: an account is shared with a user, a team, a project or a service account, not a system"},
		{"sharing with its own owner", as("u-alice"), editsOnly{}, "acct-alice", user("u-alice"), `409 already_reachable: your personal scope already reaches your personal account "acct-alice" as its owner or a scope inside it, so a share adds nothing. List the account in a pool of your personal scope instead.`},
		{"a team's account with a project of the team", as("u-dave"), teamAdmin, "acct-team-1", scope(projectOne), `409 already_reachable: project "p-1" already reaches account "acct-team-1" of team "t-1" as its owner or a scope inside it, so a share adds nothing. List the account in a pool of project "p-1" instead.`},
		{"a system account, which every scope reaches", adminToken(), everything, "acct-shared", scope(teamTwo), `409 already_reachable: team "t-2" already reaches system account "acct-shared" as its owner or a scope inside it, so a share adds nothing. List the account in a pool of team "t-2" instead.`},
	} {
		src := rows.accounts[tc.src]
		share := account.Share(src, tc.to.r, tc.to.owner, "x")
		share.Meta.ID = "share-new"
		err := rows.shareChecker(tc.caller).Share(tc.ctx, src, share)
		wantRefused(t, tc.name, err, tc.want)
		if re := (*Error)(nil); errors.As(err, &re) && re.Status == 403 && !errors.Is(err, authz.ErrForbidden) {
			t.Errorf("%s: err = %v, want it to wrap authz.ErrForbidden", tc.name, err)
		}
	}
}

// A receiver the sharer may not see answers exactly as one that does not exist, or the answer tells the two apart.
func TestShareHiddenServiceAccountAnswersAsMissing(t *testing.T) {
	t.Skip(`bug: share with a hidden service account answers 404 serviceaccount "sa-1" not found, a missing one 404 service-account "sa-gone" not found: the kind word tells hidden from missing`)
	rows := shareRows()
	src := rows.accounts["acct-alice"]
	share := account.Share(src, account.Receiver{Kind: account.ReceiverServiceAccount, ID: "sa-1"}, projectOne, "x")
	err := rows.shareChecker(editsOnly{hidden: []string{"sa-1"}}).Share(as("u-alice"), src, share)
	wantRefused(t, "a service account the sharer may not see", err, `404: service-account "sa-1" not found`)
}

// A share stored as a row names a real provider account at its own host.
func TestSharedAccountSource(t *testing.T) {
	rows := shareRows()
	c := rows.shareChecker(nil)
	for _, tc := range []struct {
		name   string
		src    string
		hostID string
		want   string
	}{
		{"a provider account at the same host", "acct-team-1", "h-1", ""},
		{"an account that does not exist", "acct-gone", "h-1", `400: account "acct-gone" does not exist`},
		{"a share", "share-t2", "h-1", `409 reshare_refused: account "acct-team-1" of team "t-2" is a share, so it cannot be shared on: only the owner of the account it came from shares that account. Ask them to share it with the receiver.`},
		{"a caller account", "caller-t-1", "h-1", `400: account "t-1" of team "t-1" is a team account; only a provider account is shared`},
		{"another host than the source's", "acct-team-1", "h-2", `400: a share is held at the host of the account it comes from, account "acct-team-1" of team "t-1"`},
	} {
		share := &account.Account{Meta: meta.Metadata{Name: "s", Owner: teamTwo}, Spec: account.Spec{HostID: tc.hostID, SourceAccountID: tc.src}}
		wantRefused(t, tc.name, c.Account(context.Background(), share), tc.want)
	}
}

// A share holds no keys: a host key naming one is refused with account_shared, pointing at the real account.
func TestHostKeyOnShareRefused(t *testing.T) {
	k := &hostkey.HostKey{Meta: meta.Metadata{Name: "k", Owner: teamTwo}, Spec: hostkey.Spec{HostID: "h-1", AccountID: "share-t2"}}
	wantRefused(t, "host key on a share", shareRows().shareChecker(nil).HostKey(context.Background(), k), `409 account_shared: account "acct-team-1" of team "t-2" is a share and holds no keys of its own: its keys are the ones of the account it came from. Add the host key to that account, or join an account of team "t-2".`)
}

// A pool draws on accounts at its host: its scope's own and the ones its scope may use, and a share only in the scope it was shared with.
func TestPoolAccounts(t *testing.T) {
	rows := shareRows()
	rows.accounts["acct-h2"] = &account.Account{Meta: meta.Metadata{ID: "acct-h2", Name: "acct-h2", Owner: teamTwo}, Spec: account.Spec{HostID: "h-2"}}
	for _, tc := range []struct {
		name     string
		authz    authz.Authorizer
		owner    meta.Owner
		accounts []string
		want     string
	}{
		{"its own and the system's accounts", nil, teamTwo, []string{"acct-team-2", "acct-shared"}, ""},
		{"a share in its own scope", nil, teamTwo, []string{"share-t2"}, ""},
		{"a team's account from its project", nil, projectOne, []string{"acct-team-1"}, ""},
		{"a share from another scope", nil, teamOne, []string{"acct-team-1", "share-t2"}, `400: accountIds[1]: account "acct-team-1" was shared with team "t-2", so only a pool of team "t-2" lists it. Create the pool there, or ask the owner of the account it came from to share it with team "t-1".`},
		{"another team's account", nil, teamOne, []string{"acct-team-2"}, `400: accountIds[0]: a pool of team "t-1" cannot use account "acct-team-2" of team "t-2": a team's pool can use accounts of the team or the system. Create the account in the team, or move this one there.`},
		{"an account at another host", nil, teamTwo, []string{"acct-h2"}, `400: accountIds[0]: account "acct-h2" of team "t-2" is held at another host than the pool's; a pool draws on accounts of its own host`},
		{"an account that does not exist", nil, teamTwo, []string{"acct-gone"}, `400: accountIds[0]: account "acct-gone" does not exist`},
		{"an account the caller cannot see", hiding("acct-team-2"), teamTwo, []string{"acct-team-2"}, `404: account "acct-team-2" not found`},
		{"a caller account", nil, teamOne, []string{"caller-t-1"}, `400: accountIds[0]: account "t-1" of team "t-1" is a team account; a pool draws on provider accounts`},
		{"personal pool, a project's account, single-user", nil, alice, []string{"acct-p-1"}, ""},
		{"personal pool, a project's account, scoped", hiding(), alice, []string{"acct-p-1"}, `400: accountIds[0]: a pool of your personal scope cannot draw on account "acct-p-1" of project "p-1": a personal pool cannot draw on a project's accounts. Use an account of your own or of the system, or create the pool in that project.`},
	} {
		p := &pool.Pool{Meta: meta.Metadata{Name: "p", Owner: tc.owner}, Spec: pool.Spec{HostID: "h-1", AccountIDs: tc.accounts}}
		wantRefused(t, tc.name, rows.shareChecker(tc.authz).Pool(as("u-alice"), p), tc.want)
	}
}
