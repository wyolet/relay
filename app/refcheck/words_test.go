package refcheck

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
)

const meID = "01a19bc3-0000-7000-8000-000000000001"

// wordRows is linkRows with display names, a personal pool and policy of meID, and a username lookup.
func wordRows() (*linkRows, Checker) {
	r := newLinkRows()
	r.teams["t-1"].Meta.DisplayName = "Platform"
	r.projects["p-1"].Meta.DisplayName = "Web"
	mine := meta.Owner{Kind: meta.OwnerUser, ID: meID}
	r.pools["pool-mine"] = &pool.Pool{Meta: meta.Metadata{ID: "pool-mine", Name: "anthropic-967841", DisplayName: "anthropic", Owner: mine}, Spec: pool.Spec{HostID: "h-1"}}
	r.accounts["acct-mine"] = &account.Account{Meta: meta.Metadata{ID: "acct-mine", Name: "acct-mine", DisplayName: "Anthropic org", Owner: mine}, Spec: account.Spec{HostID: "h-1"}}
	// Two shares, each with a scope that does not own the account already: t-1's with team t-2, and t-2's with project p-1 (in t-1).
	for id, share := range map[string]struct {
		src string
		to  meta.Owner
	}{
		"share-t-2": {"acct-team-1", meta.Owner{Kind: meta.OwnerTeam, ID: "t-2"}},
		"share-p-1": {"acct-team-2", meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}},
	} {
		s := account.Share(r.accounts[share.src], account.Receiver{Kind: account.ReceiverKind(share.to.Kind), ID: share.to.ID}, share.to, share.to.ID)
		s.Meta.ID = id
		r.accounts[id] = s
	}
	c := r.checker(nil)
	pol := &policy.Policy{Meta: meta.Metadata{ID: "pol-mine", Name: "general-policy-051141", DisplayName: "General Policy", Owner: mine}, Spec: policy.Spec{PoolIDs: []string{"pool-mine"}}}
	c.Rows.Policy = func(_ context.Context, id string) *policy.Policy {
		if id == pol.Meta.ID {
			return pol
		}
		return nil
	}
	c.Rows.UserMayBindIn = func(context.Context, string, meta.Owner) (bool, error) { return true, nil }
	c.Rows.UserName = func(_ context.Context, id string) string {
		if id == meID {
			return "abror"
		}
		return ""
	}
	return r, c
}

func refusalMsg(t *testing.T, err error) string {
	t.Helper()
	var re *Error
	if !errors.As(err, &re) {
		t.Fatalf("err = %v, want a refcheck refusal", err)
	}
	return re.Msg
}

// A refusal names the rows the way the caller picked them and says what to change, never raw ids or owner kinds.
func TestScopeRefusalsSayWhatToDo(t *testing.T) {
	_, c := wordRows()
	me := actor.WithActor(context.Background(), &actor.Actor{UserID: meID, Username: "abror"})
	admin := actor.WithActor(context.Background(), &actor.Actor{AdminToken: true})
	project := meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}
	team2 := meta.Owner{Kind: meta.OwnerTeam, ID: "t-2"}

	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{
			"a project binding uses a personal policy linking a personal pool",
			c.PolicyBinding(me, &policybinding.PolicyBinding{Meta: meta.Metadata{Name: "b", Owner: project}, Spec: policybinding.Spec{PolicyID: "pol-mine"}}),
			`policy "General Policy" uses your personal pool "anthropic"; a policy binding of project "Web" can use pools of the project, its team or the system. Create the pool in the project or its team, or move this one there.`,
		},
		{
			"the same, read by the admin token",
			c.PolicyBinding(admin, &policybinding.PolicyBinding{Meta: meta.Metadata{Name: "b", Owner: project}, Spec: policybinding.Spec{PolicyID: "pol-mine"}}),
			`policy "General Policy" uses abror's personal pool "anthropic"; a policy binding of project "Web" can use pools of the project, its team or the system. Create the pool in the project or its team, or move this one there.`,
		},
		{
			"a team's policy links another team's pool",
			c.Policy(me, &policy.Policy{Meta: meta.Metadata{Name: "x", Owner: team2}, Spec: policy.Spec{PoolIDs: []string{"pool-team-1"}}}),
			`a policy of team "t-2" cannot use pool "pool-team-1" of team "Platform": a team's policy can use pools of the team or the system. Create the pool in the team, or move this one there.`,
		},
		{
			"a project's host key joins a personal account",
			c.HostKey(me, &hostkey.HostKey{Meta: meta.Metadata{Name: "k", Owner: project}, Spec: hostkey.Spec{HostID: "h-1", AccountID: "acct-mine"}}),
			`a host key of project "Web" cannot join your personal account "Anthropic org": a host key joins only an account of its own owner. Create the host key in your personal scope, or pick an account of project "Web".`,
		},
		{
			"a project binding binds a personal account",
			c.PolicyBinding(me, &policybinding.PolicyBinding{Meta: meta.Metadata{Name: "b", Owner: project}, Spec: policybinding.Spec{AccountIDs: []string{"acct-mine"}}}),
			`accountIds[0]: your personal account "Anthropic org" is outside the scope of this binding's owner, project "Web": a project's binding binds the project's own accounts: its account, its members', its service accounts' and its provider accounts. Create the binding in your personal scope instead.`,
		},
		{
			"a team's pool lists a share made with a project",
			c.Pool(me, &pool.Pool{Meta: meta.Metadata{Name: "x", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"}}, Spec: pool.Spec{HostID: "h-1", AccountIDs: []string{"share-p-1"}}}),
			`accountIds[0]: account "acct-team-2" was shared with project "Web", so only a pool of project "Web" lists it. Create the pool there, or ask the owner of the account it came from to share it with team "Platform".`,
		},
		{
			"sharing a share on",
			c.Share(me, c.Rows.Account(me, "share-p-1"), account.Share(c.Rows.Account(me, "share-p-1"), account.Receiver{Kind: account.ReceiverTeam, ID: "t-2"}, team2, "t-2")),
			`account "acct-team-2" of project "Web" is a share, so it cannot be shared on: only the owner of the account it came from shares that account. Ask them to share it with the receiver.`,
		},
		{
			"a host key joins a share",
			c.HostKey(me, &hostkey.HostKey{Meta: meta.Metadata{Name: "k", Owner: project}, Spec: hostkey.Spec{HostID: "h-1", AccountID: "share-p-1"}}),
			`account "acct-team-2" of project "Web" is a share and holds no keys of its own: its keys are the ones of the account it came from. Add the host key to that account, or join an account of project "Web".`,
		},
		{
			"a second share with one receiver",
			c.ShareExists(me, c.Rows.Account(me, "acct-team-2"), account.Receiver{Kind: account.ReceiverProject, ID: "p-1"}),
			`account "acct-team-2" of team "t-2" is already shared with project "Web": one share per receiver. Use the existing share, or delete it first.`,
		},
		{
			"sharing with a project of the owning team",
			c.Share(me, c.Rows.Account(me, "acct-team-1"), account.Share(c.Rows.Account(me, "acct-team-1"), account.Receiver{Kind: account.ReceiverProject, ID: "p-1"}, project, "p-1")),
			`project "Web" already reaches account "acct-team-1" of team "Platform" as its owner or a scope inside it, so a share adds nothing. List the account in a pool of project "Web" instead.`,
		},
	} {
		got := refusalMsg(t, tc.err)
		if got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, got, tc.want)
		}
		for _, raw := range []string{meID, "p-1", "t-1", "user ", "team t", "project p"} {
			if strings.Contains(got, raw) {
				t.Errorf("%s: message names %q: %s", tc.name, raw, got)
			}
		}
	}
}

// The usable_by predicates are the save rules: for every row and scope, a list filtered by one keeps exactly what the linking row's save accepts.
func TestUsableMatchesTheSaveRule(t *testing.T) {
	r, c := wordRows()
	ctx := actor.WithActor(context.Background(), &actor.Actor{UserID: meID})
	scopes := []meta.Owner{
		{Kind: meta.OwnerSystem}, {Kind: meta.OwnerUser, ID: meID},
		{Kind: meta.OwnerTeam, ID: "t-1"}, {Kind: meta.OwnerTeam, ID: "t-2"},
		{Kind: meta.OwnerProject, ID: "p-1"}, {Kind: meta.OwnerProject, ID: "p-2"},
	}
	for _, by := range scopes {
		for id, p := range r.pools {
			usable := c.UsablePool(ctx, p, by) == nil
			saved := c.Policy(ctx, &policy.Policy{Meta: meta.Metadata{Name: "x", Owner: by}, Spec: policy.Spec{PoolIDs: []string{id}}}) == nil
			if usable != saved {
				t.Errorf("pool %s by %v: usable %v, policy save %v", id, by, usable, saved)
			}
		}
		for id, k := range r.keys {
			usable := c.UsableHostKey(ctx, k, by) == nil
			saved := c.Pool(ctx, &pool.Pool{Meta: meta.Metadata{Name: "x", Owner: by}, Spec: pool.Spec{HostID: k.Spec.HostID, CredentialIDs: []string{id}}}) == nil
			if usable != saved {
				t.Errorf("host key %s by %v: usable %v, pool save %v", id, by, usable, saved)
			}
		}
		for id, a := range r.accounts {
			usable := c.UsableAccount(ctx, a, by) == nil
			saved := c.PolicyBinding(ctx, &policybinding.PolicyBinding{Meta: meta.Metadata{Name: "x", Owner: by}, Spec: policybinding.Spec{AccountIDs: []string{id}}}) == nil
			if usable != saved {
				t.Errorf("account %s by %v: usable %v, binding save %v", id, by, usable, saved)
			}
			usable = c.UsablePoolAccount(ctx, a, by) == nil
			saved = c.Pool(ctx, &pool.Pool{Meta: meta.Metadata{Name: "x", Owner: by}, Spec: pool.Spec{HostID: a.Spec.HostID, AccountIDs: []string{id}}}) == nil
			if usable != saved {
				t.Errorf("account %s by %v: usable for a pool %v, pool save %v", id, by, usable, saved)
			}
		}
		pol := c.Rows.Policy(ctx, "pol-mine")
		usable := c.UsablePolicy(ctx, pol, by) == nil
		saved := c.PolicyBinding(ctx, &policybinding.PolicyBinding{Meta: meta.Metadata{Name: "x", Owner: by}, Spec: policybinding.Spec{PolicyID: pol.Meta.ID}}) == nil
		if usable != saved {
			t.Errorf("policy by %v: usable %v, binding save %v", by, usable, saved)
		}
	}
	t2 := meta.Owner{Kind: meta.OwnerTeam, ID: "t-2"}
	if err := c.UsablePoolAccount(ctx, r.accounts["share-t-2"], t2); err != nil {
		t.Errorf("a pool of the receiving team may not list its share: %v", err)
	}
	if err := c.UsablePoolAccount(ctx, r.accounts["share-p-1"], meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"}); err == nil {
		t.Error("a pool of another scope may list a share")
	}
	if err := c.UsablePool(ctx, r.pools["pool-mine"], meta.Owner{Kind: meta.OwnerUser, ID: meID}); err != nil {
		t.Errorf("a personal policy may not link its owner's pool: %v", err)
	}
	if err := c.UsablePool(ctx, r.pools["pool-mine"], meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}); err == nil {
		t.Error("a project's policy may link a personal pool")
	}
	if err := c.UsablePool(ctx, r.pools["pool-team-1"], meta.Owner{Kind: meta.OwnerProject, ID: "p-2"}); err == nil {
		t.Error("another team's project may link team t-1's pool")
	}
}
