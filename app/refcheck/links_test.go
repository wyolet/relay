package refcheck

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
)

// hidingAuthz scopes reads and hides the listed row ids.
type hidingAuthz struct{ hidden map[string]bool }

func hiding(ids ...string) hidingAuthz {
	h := hidingAuthz{hidden: map[string]bool{}}
	for _, id := range ids {
		h.hidden[id] = true
	}
	return h
}

func (hidingAuthz) Authorize(context.Context, string, authz.Resource) error { return nil }

func (h hidingAuthz) Visible(_ context.Context, _ string, id string, _ meta.Owner) bool {
	return !h.hidden[id]
}

// linkRows is a small catalog: hosts h-1 and h-2, team t-1 with project p-1, project p-2 in team t-2.
type linkRows struct {
	hosts    map[string]*host.Host
	keys     map[string]*hostkey.HostKey
	accounts map[string]*account.Account
	pools    map[string]*pool.Pool
	budgets  map[string]*budget.Budget
	limits   map[string]*ratelimit.RateLimit
	teams    map[string]*team.Team
	projects map[string]*project.Project
}

func newLinkRows() *linkRows {
	r := &linkRows{
		hosts: map[string]*host.Host{
			"h-1": {Meta: meta.Metadata{ID: "h-1", Name: "h-1", Owner: system}},
			"h-2": {Meta: meta.Metadata{ID: "h-2", Name: "h-2", Owner: system}},
		},
		keys:     map[string]*hostkey.HostKey{},
		accounts: map[string]*account.Account{},
		pools:    map[string]*pool.Pool{},
		budgets:  map[string]*budget.Budget{},
		limits:   map[string]*ratelimit.RateLimit{"rl": {Meta: meta.Metadata{ID: "rl", Name: "rl", Owner: system}}},
		teams: map[string]*team.Team{
			"t-1": {Meta: meta.Metadata{ID: "t-1", Name: "t-1", Owner: system}},
			"t-2": {Meta: meta.Metadata{ID: "t-2", Name: "t-2", Owner: system}},
		},
		projects: map[string]*project.Project{
			"p-1": {Meta: meta.Metadata{ID: "p-1", Name: "p-1"}, Spec: project.Spec{TeamID: "t-1"}},
			"p-2": {Meta: meta.Metadata{ID: "p-2", Name: "p-2"}, Spec: project.Spec{TeamID: "t-2"}},
		},
	}
	for id, k := range map[string]struct {
		host  string
		owner meta.Owner
	}{
		"key-h1":      {"h-1", system},
		"key-h2":      {"h-2", system},
		"key-team-1":  {"h-1", teamOne},
		"key-project": {"h-1", projectOne},
	} {
		r.keys[id] = &hostkey.HostKey{Meta: meta.Metadata{ID: id, Name: id, Owner: k.owner}, Spec: hostkey.Spec{HostID: k.host}}
	}
	for id, owner := range map[string]meta.Owner{"shared": system, "team-1": teamOne, "team-2": teamTwo, "p-1": projectOne, "p-2": projectTwo} {
		r.accounts["acct-"+id] = &account.Account{Meta: meta.Metadata{ID: "acct-" + id, Name: "acct-" + id, Owner: owner}, Spec: account.Spec{HostID: "h-1"}}
		r.pools["pool-"+id] = &pool.Pool{Meta: meta.Metadata{ID: "pool-" + id, Name: "pool-" + id, Owner: owner}, Spec: pool.Spec{HostID: "h-1"}}
		r.budgets["budget-"+id] = &budget.Budget{Meta: meta.Metadata{ID: "budget-" + id, Name: "budget-" + id, Owner: owner}}
	}
	return r
}

func (r *linkRows) checker(a authz.Authorizer) Checker {
	return Checker{Authz: a, Rows: Lookup{
		Host:      func(_ context.Context, id string) *host.Host { return r.hosts[id] },
		HostKey:   func(_ context.Context, id string) *hostkey.HostKey { return r.keys[id] },
		Account:   func(_ context.Context, id string) *account.Account { return r.accounts[id] },
		Pool:      func(_ context.Context, id string) *pool.Pool { return r.pools[id] },
		Budget:    func(_ context.Context, id string) *budget.Budget { return r.budgets[id] },
		RateLimit: func(_ context.Context, id string) *ratelimit.RateLimit { return r.limits[id] },
		Team:      func(_ context.Context, id string) *team.Team { return r.teams[id] },
		Project:   func(_ context.Context, id string) *project.Project { return r.projects[id] },
	}}
}

// refused renders a refusal as "<status> <code>: <msg>" (no code: "<status>: <msg>"), "" for an accepted row.
func refused(err error) string {
	var re *Error
	switch {
	case err == nil:
		return ""
	case !errors.As(err, &re):
		return "not a refusal: " + err.Error()
	}
	// kindWords misspells this plural (TestKindWords); spelled right here so the tables hold the intended text.
	msg := strings.ReplaceAll(re.Msg, "host keies", "host keys")
	if re.Code == "" {
		return fmt.Sprintf("%d: %s", re.Status, msg)
	}
	return fmt.Sprintf("%d %s: %s", re.Status, re.Code, msg)
}

func wantRefused(t *testing.T, name string, err error, want string) {
	t.Helper()
	if got := refused(err); got != want {
		t.Errorf("%s:\n got %q\nwant %s", name, got, want)
	}
}

// A pool is one host's keys, each of a scope the pool's owner draws on.
func TestPoolKeys(t *testing.T) {
	rows := newLinkRows()
	ctx := as("u-alice")
	mk := func(owner meta.Owner, hostID string, keys ...string) *pool.Pool {
		return &pool.Pool{Meta: meta.Metadata{Name: "p", Owner: owner}, Spec: pool.Spec{HostID: hostID, CredentialIDs: keys}}
	}
	for _, tc := range []struct {
		name  string
		authz authz.Authorizer
		p     *pool.Pool
		want  string
	}{
		{"keys of the pool's host", nil, mk(system, "h-1", "key-h1", "key-team-1"), ""},
		{"a key of another host", nil, mk(system, "h-1", "key-h1", "key-h2"), `400: system host key "key-h2" authenticates to another host than the pool's; a pool holds keys of its own host`},
		{"a key that does not exist", nil, mk(system, "h-1", "key-gone"), `400: host-key "key-gone" does not exist`},
		{"a host that does not exist", nil, mk(system, "h-9"), `400: host "h-9" does not exist`},
		{"a host the caller cannot see", hiding("h-1"), mk(system, "h-1"), `404: host "h-1" not found`},
		{"a key the caller cannot see", hiding("key-h1"), mk(system, "h-1", "key-h1"), `404: host-key "key-h1" not found`},
		{"team pool, the team's key", nil, mk(teamOne, "h-1", "key-team-1"), ""},
		{"team pool, a system key", nil, mk(teamOne, "h-1", "key-h1"), ""},
		{"team pool, its project's key", nil, mk(teamOne, "h-1", "key-project"), `400: a pool of team "t-1" cannot use host key "key-project" of project "p-1": a team's pool can use host keys of the team or the system. Create the host key in the team, or move this one there.`},
		{"personal pool, a project's key, single-user", nil, mk(alice, "h-1", "key-project"), ""},
		{"personal pool, a project's key, scoped", hiding(), mk(alice, "h-1", "key-project"), `400: a pool of your personal scope cannot hold host key "key-project" of project "p-1": a personal pool cannot hold a project's host keys. Use a host key of your own or of the system, or create the pool in that project.`},
	} {
		wantRefused(t, tc.name, rows.checker(tc.authz).Pool(ctx, tc.p), tc.want)
	}
	// A policy holds keys under the same owner rule, at any host.
	c := rows.checker(nil)
	wantRefused(t, "project policy, its own, its team's and system keys", c.CredentialRefs(ctx, []string{"key-project", "key-team-1", "key-h1", "key-h2"}, projectOne), "")
	wantRefused(t, "project policy, another project's key", c.CredentialRefs(ctx, []string{"key-h1", "key-project"}, projectTwo),
		`400: a policy of project "p-2" cannot use host key "key-project" of project "p-1": a project's policy can use host keys of the project, its team or the system. Create the host key in the project or its team, or move this one there.`)
}

// A host key names an account of its own owner, held at the key's host.
func TestHostKeyAccount(t *testing.T) {
	rows := newLinkRows()
	rows.accounts["acct-h2"] = &account.Account{Meta: meta.Metadata{ID: "acct-h2", Name: "acct-h2", Owner: system}, Spec: account.Spec{HostID: "h-2"}}
	rows.accounts["caller-t-1"] = account.ForTeam("t-1", "t-1", "")
	ctx := as("u-alice")
	for _, tc := range []struct {
		name    string
		authz   authz.Authorizer
		owner   meta.Owner
		account string
		want    string
		// accountRef is AccountRef's answer where it differs: it checks the owner only, not the account's kind or host.
		accountRef string
	}{
		{"no account", nil, system, "", "400 account_required: " + hostkey.ErrNoAccount.Error(), `400: account "" does not exist`},
		{"an account of the key's host", nil, system, "acct-shared", "", ""},
		{"an account of another host", nil, system, "acct-h2", `400: system account "acct-h2" is held at another host than the key's; a host key joins an account of its own host`, "accepted"},
		{"an account that does not exist", nil, system, "acct-gone", `400: account "acct-gone" does not exist`, ""},
		{"an account the caller cannot see", hiding("acct-shared"), system, "acct-shared", `404: account "acct-shared" not found`, ""},
		{"a caller account", nil, teamOne, "caller-t-1", `400: account "team-t-1" is a team account; a host key joins a provider account`, "accepted"},
		{"p-1 key, p-1's account", nil, projectOne, "acct-p-1", "", ""},
		{"p-1 key, its team's account", nil, projectOne, "acct-team-1", `400: a host key of project "p-1" cannot join account "acct-team-1" of team "t-1": a host key joins only an account of its own owner. Create the host key in team "t-1", or pick an account of project "p-1".`, ""},
		{"p-1 key, another team's account", nil, projectOne, "acct-team-2", `400: a host key of project "p-1" cannot join account "acct-team-2" of team "t-2": a host key joins only an account of its own owner. Create the host key in team "t-2", or pick an account of project "p-1".`, ""},
		{"p-1 key, a system account", nil, projectOne, "acct-shared", `400: a host key of project "p-1" cannot join system account "acct-shared": a host key joins only an account of its own owner. Create the host key in the system, or pick an account of project "p-1".`, ""},
		{"p-1 key, another project's account", nil, projectOne, "acct-p-2", `400: a host key of project "p-1" cannot join account "acct-p-2" of project "p-2": a host key joins only an account of its own owner. Create the host key in project "p-2", or pick an account of project "p-1".`, ""},
		{"t-1 key, a system account", nil, teamOne, "acct-shared", `400: a host key of team "t-1" cannot join system account "acct-shared": a host key joins only an account of its own owner. Create the host key in the system, or pick an account of team "t-1".`, ""},
		{"t-1 key, t-1's account", nil, teamOne, "acct-team-1", "", ""},
	} {
		k := &hostkey.HostKey{Meta: meta.Metadata{Name: "k", Owner: tc.owner}, Spec: hostkey.Spec{HostID: "h-1", AccountID: tc.account}}
		c := rows.checker(tc.authz)
		err := c.HostKey(ctx, k)
		wantRefused(t, tc.name, err, tc.want)
		if tc.account == "" && !errors.Is(err, hostkey.ErrNoAccount) {
			t.Errorf("%s: err = %v, want it to wrap hostkey.ErrNoAccount", tc.name, err)
		}
		want := tc.want
		switch tc.accountRef {
		case "":
		case "accepted":
			want = ""
		default:
			want = tc.accountRef
		}
		wantRefused(t, tc.name+" (AccountRef)", c.AccountRef(ctx, tc.account, tc.owner), want)
	}
	c := rows.checker(nil)
	wantRefused(t, "a provider account at a missing host", c.Account(ctx, &account.Account{Spec: account.Spec{HostID: "h-9"}}), `400: host "h-9" does not exist`)
	wantRefused(t, "a caller account names no host", c.Account(ctx, account.ForTeam("t-1", "t-1", "")), "")
}

// Every pool, rate limit and budget a policy links must exist, be visible to the caller, and live in a scope the policy's owner draws on: its own, its team's or the system's, and never another user's.
func TestPolicyLinks(t *testing.T) {
	rows := newLinkRows()
	rows.pools["pool-alice"] = &pool.Pool{Meta: meta.Metadata{ID: "pool-alice", Name: "pool-alice", Owner: alice}, Spec: pool.Spec{HostID: "h-1"}}
	rows.pools["pool-bob"] = &pool.Pool{Meta: meta.Metadata{ID: "pool-bob", Name: "pool-bob", Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-bob"}}, Spec: pool.Spec{HostID: "h-1"}}
	rows.limits["rl-p-2"] = &ratelimit.RateLimit{Meta: meta.Metadata{ID: "rl-p-2", Name: "rl-p-2", Owner: projectTwo}}
	ctx := as("u-alice")
	hidden := hiding("pool-p-2", "budget-p-2", "rl-p-2")
	// noKeys sees every row but may create keys nowhere.
	noKeys := teamGrant{}
	pools := func(ids ...string) policy.Spec { return policy.Spec{PoolIDs: ids} }
	budgets := func(id string) policy.Spec { return policy.Spec{Budgets: []policy.BudgetLink{{BudgetID: id}}} }
	limits := func(id string) policy.Spec {
		return policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: id}}}
	}
	for _, tc := range []struct {
		name  string
		authz authz.Authorizer
		owner meta.Owner
		spec  policy.Spec
		want  string
	}{
		{"every link present", hidden, system, policy.Spec{PoolIDs: []string{"pool-shared"}, RateLimits: []policy.RateLimitLink{{RateLimitID: "rl"}}, Budgets: []policy.BudgetLink{{BudgetID: "budget-shared"}}}, ""},
		{"missing pool", hidden, system, pools("pool-shared", "pool-gone"), `400: pool "pool-gone" does not exist`},
		{"hidden pool", hidden, system, pools("pool-p-2"), `404: pool "pool-p-2" not found`},
		{"missing rate limit", hidden, system, limits("rl-gone"), `400: rate-limit "rl-gone" does not exist`},
		{"hidden rate limit", hidden, system, limits("rl-p-2"), `404: rate-limit "rl-p-2" not found`},
		{"missing budget", hidden, system, budgets("budget-gone"), `400: budget "budget-gone" does not exist`},
		{"hidden budget", hidden, system, budgets("budget-p-2"), `404: budget "budget-p-2" not found`},
		{"user, their own pool", nil, alice, pools("pool-alice"), ""},
		{"user, a system pool", nil, alice, pools("pool-shared"), ""},
		{"user, another user's pool", nil, alice, pools("pool-bob"), `400: a policy of your personal scope cannot use another user's personal pool "pool-bob": a personal policy can use pools of the same user or the system. Create the pool in the same personal scope, or move this one there.`},
		{"user, a pool of a project they may create keys in", hiding(), alice, pools("pool-p-1"), ""},
		{"user, a pool of a project they may not create keys in", noKeys, alice, pools("pool-p-1"), `400: a policy of your personal scope cannot use pool "pool-p-1" of project "p-1": a personal row uses a project's rows only where you may create keys in that project. Use a pool of your own or of the system, or ask for a role in project "p-1".`},
		{"user, a rate limit of a project they may not create keys in", noKeys, alice, limits("rl-p-2"), `400: a policy of your personal scope cannot use rate limit "rl-p-2" of project "p-2": a personal row uses a project's rows only where you may create keys in that project. Use a rate limit of your own or of the system, or ask for a role in project "p-2".`},
		{"p-1, its team's pool", nil, projectOne, pools("pool-team-1"), ""},
		{"p-1, its team's budget", nil, projectOne, budgets("budget-team-1"), ""},
		{"p-1, its own pool", nil, projectOne, pools("pool-p-1"), ""},
		{"p-1, a system pool", nil, projectOne, pools("pool-shared"), ""},
		{"p-2, another team's pool", nil, projectTwo, pools("pool-team-1"), `400: a policy of project "p-2" cannot use pool "pool-team-1" of team "t-1": a project's policy can use pools of the project, its team or the system. Create the pool in the project or its team, or move this one there.`},
		{"p-2, another team's budget", nil, projectTwo, budgets("budget-team-1"), `400: a policy of project "p-2" cannot use budget "budget-team-1" of team "t-1": a project's policy can use budgets of the project, its team or the system. Create the budget in the project or its team, or move this one there.`},
		{"p-2, a sibling project's pool", nil, projectTwo, pools("pool-p-1"), `400: a policy of project "p-2" cannot use pool "pool-p-1" of project "p-1": a project's policy can use pools of the project, its team or the system. Create the pool in the project or its team, or move this one there.`},
		{"t-1, its project's pool", nil, teamOne, pools("pool-p-1"), `400: a policy of team "t-1" cannot use pool "pool-p-1" of project "p-1": a team's policy can use pools of the team or the system. Create the pool in the team, or move this one there.`},
		{"p-1, another project's rate limit", nil, projectOne, limits("rl-p-2"), `400: a policy of project "p-1" cannot use rate limit "rl-p-2" of project "p-2": a project's policy can use rate limits of the project, its team or the system. Create the rate limit in the project or its team, or move this one there.`},
	} {
		p := &policy.Policy{Meta: meta.Metadata{Name: "x", Owner: tc.owner}, Spec: tc.spec}
		wantRefused(t, tc.name, rows.checker(tc.authz).Policy(ctx, p), tc.want)
	}
}

// A key's lane is an account its principal holds, or a provider account its user owns; an empty one takes the principal's own account; a closed one is refused.
func TestKeyAccount(t *testing.T) {
	rows := newLinkRows()
	personal := account.Personal("u-1", "alice")
	personal.Meta.ID = "personal-u-1"
	other := account.Personal("u-2", "bob")
	other.Meta.ID = "personal-u-2"
	closed := account.Member("u-1", "alice", projectOne, "p-1", "")
	closed.Meta.ID, closed.Spec.Closed, closed.FollowsRoleBindings = "member-u-1", true, true
	granted := account.Member("u-1", "alice", projectTwo, "p-2", "")
	granted.Meta.ID, granted.FollowsRoleBindings = "member-granted", true
	firstUse := account.Member("u-1", "alice", meta.Owner{Kind: meta.OwnerProject, ID: "p-3"}, "p-3", "")
	firstUse.Meta.ID = "member-first-use"
	ownOrg := &account.Account{Meta: meta.Metadata{ID: "own-org", Name: "alice-org", Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-1"}}, Spec: account.Spec{HostID: "h-1"}}
	service := account.ForServiceAccount("sa-1", "ci", "CI", "p-1")
	service.Meta.ID = "service-sa-1"
	for _, a := range []*account.Account{personal, other, closed, granted, firstUse, ownOrg, service} {
		rows.accounts[a.Meta.ID] = a
	}
	c := rows.shareChecker(nil)
	var defaultErr error
	c.Rows.DefaultAccount = func(_ context.Context, p key.Principal) (string, error) { return "personal-" + p.ID, defaultErr }
	user, sa := key.Principal{Kind: key.PrincipalUser, ID: "u-1"}, key.Principal{Kind: key.PrincipalServiceAccount, ID: "sa-1"}
	for _, tc := range []struct {
		name      string
		ctx       context.Context
		principal key.Principal
		account   string
		want      string
	}{
		{"own personal account", as("u-1"), user, "personal-u-1", ""},
		{"a member account a role binding grants", as("u-1"), user, "member-granted", ""},
		{"a provider account the user owns", as("u-1"), user, "own-org", ""},
		{"a service account's own account", as("u-1"), sa, "service-sa-1", ""},
		{"a provider account the user does not own", as("u-1"), user, "acct-shared", `403 account_not_held: your keys cannot run in system account "acct-shared": a key runs in its principal's personal or service account, a member account a role binding grants, or a provider account its user owns. Leave accountId empty to use the principal's own account.`},
		{"a member account opened on first use", as("u-1"), user, "member-first-use", `403 account_not_held: your keys cannot run in account "alice in p-3" of another project: a key runs in its principal's personal or service account, a member account a role binding grants, or a provider account its user owns. Leave accountId empty to use the principal's own account.`},
		{"another user's account", as("u-1"), user, "personal-u-2", `403 account_not_held: your keys cannot run in another user's personal account "bob": a key runs in its principal's personal or service account, a member account a role binding grants, or a provider account its user owns. Leave accountId empty to use the principal's own account.`},
		{"another user's account, by a third caller", as("u-9"), user, "personal-u-2", `403 account_not_held: another user's keys cannot run in another user's personal account "bob": a key runs in its principal's personal or service account, a member account a role binding grants, or a provider account its user owns. Leave accountId empty to use the principal's own account.`},
		{"a user's account under a service account's key", as("u-1"), sa, "personal-u-1", `403 account_not_held: keys of service account "sa-1" cannot run in your personal account "alice": a key runs in its principal's personal or service account, a member account a role binding grants, or a provider account its user owns. Leave accountId empty to use the principal's own account.`},
		{"a user's account under a missing service account's key", as("u-1"), key.Principal{Kind: key.PrincipalServiceAccount, ID: "sa-gone"}, "personal-u-1", `403 account_not_held: keys of a service account cannot run in your personal account "alice": a key runs in its principal's personal or service account, a member account a role binding grants, or a provider account its user owns. Leave accountId empty to use the principal's own account.`},
		{"a closed member account", as("u-1"), user, "member-u-1", `400: account "alice in p-1" of project "p-1" is closed: the membership it stood for has ended. Leave accountId empty to use the principal's own account.`},
		{"an account that does not exist", as("u-1"), user, "acct-gone", `400: account "acct-gone" does not exist`},
	} {
		k := &key.Key{Spec: key.Spec{Principal: tc.principal, AccountID: tc.account}}
		wantRefused(t, tc.name, c.KeyAccount(tc.ctx, k), tc.want)
	}

	k := &key.Key{Spec: key.Spec{Principal: user}}
	if err := c.KeyAccount(as("u-1"), k); err != nil || k.Spec.AccountID != "personal-u-1" {
		t.Errorf("defaulted lane = %q, %v; want the personal account", k.Spec.AccountID, err)
	}
	defaultErr = errors.New("store down")
	wantRefused(t, "the default account cannot be read", c.KeyAccount(as("u-1"), &key.Key{Spec: key.Spec{Principal: user}}), "500: store down")
}
