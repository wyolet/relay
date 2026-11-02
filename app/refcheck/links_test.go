package refcheck

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/team"
)

// hidingAuthz scopes reads and hides the listed row ids.
type hidingAuthz struct{ hidden map[string]bool }

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
	system := meta.Owner{Kind: meta.OwnerSystem}
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
		"key-team-1":  {"h-1", meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"}},
		"key-project": {"h-1", meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}},
	} {
		r.keys[id] = &hostkey.HostKey{Meta: meta.Metadata{ID: id, Name: id, Owner: k.owner}, Spec: hostkey.Spec{HostID: k.host}}
	}
	for id, owner := range map[string]meta.Owner{
		"shared": system,
		"team-1": {Kind: meta.OwnerTeam, ID: "t-1"},
		"team-2": {Kind: meta.OwnerTeam, ID: "t-2"},
		"p-1":    {Kind: meta.OwnerProject, ID: "p-1"},
		"p-2":    {Kind: meta.OwnerProject, ID: "p-2"},
	} {
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

func status(err error) int {
	var re *Error
	if errors.As(err, &re) {
		return re.Status
	}
	return 0
}

// A pool is one host's keys; a key of another host, or one that does not exist, is refused.
func TestPoolKeysAuthenticateToThePoolsHost(t *testing.T) {
	c := newLinkRows().checker(nil)
	ctx := context.Background()
	mk := func(hostID string, keys ...string) *pool.Pool {
		return &pool.Pool{Meta: meta.Metadata{Name: "p", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: pool.Spec{HostID: hostID, CredentialIDs: keys}}
	}
	for _, tc := range []struct {
		name string
		p    *pool.Pool
		want int // 0 = accepted
	}{
		{"keys of the pool's host", mk("h-1", "key-h1", "key-team-1"), 0},
		{"a key of another host", mk("h-1", "key-h1", "key-h2"), http.StatusBadRequest},
		{"a key that does not exist", mk("h-1", "key-gone"), http.StatusBadRequest},
		{"a host that does not exist", mk("h-9"), http.StatusBadRequest},
	} {
		if got := status(c.Pool(ctx, tc.p)); got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
}

// An account is held at one host; a key may join only an account of its own host.
func TestHostKeyAccountIsAtTheKeysHost(t *testing.T) {
	rows := newLinkRows()
	rows.accounts["acct-h2"] = &account.Account{Meta: meta.Metadata{ID: "acct-h2", Name: "acct-h2", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: account.Spec{HostID: "h-2"}}
	c := rows.checker(nil)
	ctx := context.Background()
	for _, tc := range []struct {
		name, account string
		want          int
	}{
		{"no account", "", 0},
		{"an account of the key's host", "acct-shared", 0},
		{"an account of another host", "acct-h2", http.StatusBadRequest},
		{"an account that does not exist", "acct-gone", http.StatusBadRequest},
	} {
		k := &hostkey.HostKey{
			Meta: meta.Metadata{Name: "k", Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: hostkey.Spec{HostID: "h-1", AccountID: tc.account},
		}
		if got := status(c.HostKey(ctx, k)); got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
	if got := status(c.Account(ctx, &account.Account{Spec: account.Spec{HostID: "h-9"}})); got != http.StatusBadRequest {
		t.Errorf("account at a missing host: status %d, want 400", got)
	}
}

// Every pool, rate limit and budget a policy links must exist; one the caller may not see answers as absent without saying so.
func TestPolicyLinksMustExist(t *testing.T) {
	rows := newLinkRows()
	ctx := actor.WithActor(context.Background(), &actor.Actor{UserID: "u-1"})
	c := rows.checker(hidingAuthz{hidden: map[string]bool{"pool-p-2": true}})
	full := func() *policy.Policy {
		return &policy.Policy{
			Meta: meta.Metadata{Name: "ml", Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: policy.Spec{
				PoolIDs:    []string{"pool-shared"},
				RateLimits: []policy.RateLimitLink{{RateLimitID: "rl"}},
				Budgets:    []policy.BudgetLink{{BudgetID: "budget-shared"}},
			},
		}
	}
	if err := c.Policy(ctx, full()); err != nil {
		t.Fatalf("every link present: %v", err)
	}
	for _, tc := range []struct {
		name string
		edit func(p *policy.Policy)
		want int
	}{
		{"missing pool", func(p *policy.Policy) { p.Spec.PoolIDs = []string{"pool-gone"} }, http.StatusBadRequest},
		{"hidden pool", func(p *policy.Policy) { p.Spec.PoolIDs = []string{"pool-p-2"} }, http.StatusNotFound},
		{"missing rate limit", func(p *policy.Policy) { p.Spec.RateLimits = []policy.RateLimitLink{{RateLimitID: "rl-gone"}} }, http.StatusBadRequest},
		{"missing budget", func(p *policy.Policy) { p.Spec.Budgets = []policy.BudgetLink{{BudgetID: "budget-gone"}} }, http.StatusBadRequest},
	} {
		p := full()
		tc.edit(p)
		if got := status(c.Policy(ctx, p)); got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
}

// A team's pools, budgets and accounts serve the team and its projects; another team's project, or a sibling project's rows, stay out of reach.
func TestTeamRowsServeTheTeamAndItsProjects(t *testing.T) {
	c := newLinkRows().checker(nil)
	ctx := context.Background()
	projectPolicy := func(projectID string, edit func(*policy.Policy)) *policy.Policy {
		p := &policy.Policy{Meta: meta.Metadata{Name: "x", Owner: meta.Owner{Kind: meta.OwnerProject, ID: projectID}}}
		edit(p)
		return p
	}
	for _, tc := range []struct {
		name string
		p    *policy.Policy
		ok   bool
	}{
		{"p-1 -> its team's pool", projectPolicy("p-1", func(p *policy.Policy) { p.Spec.PoolIDs = []string{"pool-team-1"} }), true},
		{"p-1 -> its team's budget", projectPolicy("p-1", func(p *policy.Policy) { p.Spec.Budgets = []policy.BudgetLink{{BudgetID: "budget-team-1"}} }), true},
		{"p-1 -> its own pool", projectPolicy("p-1", func(p *policy.Policy) { p.Spec.PoolIDs = []string{"pool-p-1"} }), true},
		{"p-1 -> a shared pool", projectPolicy("p-1", func(p *policy.Policy) { p.Spec.PoolIDs = []string{"pool-shared"} }), true},
		{"p-2 -> another team's pool", projectPolicy("p-2", func(p *policy.Policy) { p.Spec.PoolIDs = []string{"pool-team-1"} }), false},
		{"p-2 -> another team's budget", projectPolicy("p-2", func(p *policy.Policy) { p.Spec.Budgets = []policy.BudgetLink{{BudgetID: "budget-team-1"}} }), false},
		{"p-2 -> a sibling project's pool", projectPolicy("p-2", func(p *policy.Policy) { p.Spec.PoolIDs = []string{"pool-p-1"} }), false},
	} {
		if err := c.Policy(ctx, tc.p); (err == nil) != tc.ok {
			t.Errorf("%s: err=%v, want ok=%v", tc.name, err, tc.ok)
		}
	}

	teamPool := func(keys ...string) *pool.Pool {
		return &pool.Pool{Meta: meta.Metadata{Name: "tp", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"}}, Spec: pool.Spec{HostID: "h-1", CredentialIDs: keys}}
	}
	for _, tc := range []struct {
		name string
		p    *pool.Pool
		ok   bool
	}{
		{"team pool -> the team's key", teamPool("key-team-1"), true},
		{"team pool -> a shared key", teamPool("key-h1"), true},
		{"team pool -> a project's key", teamPool("key-project"), false},
	} {
		if err := c.Pool(ctx, tc.p); (err == nil) != tc.ok {
			t.Errorf("%s: err=%v, want ok=%v", tc.name, err, tc.ok)
		}
	}

	projectKey := func(acct string) *hostkey.HostKey {
		return &hostkey.HostKey{Meta: meta.Metadata{Name: "k", Owner: meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}}, Spec: hostkey.Spec{HostID: "h-1", AccountID: acct}}
	}
	// A key joins only an account of its own owner: joining one shares its counters and its 429 cooldown with every other key in it.
	for _, tc := range []struct {
		name, account string
		ok            bool
	}{
		{"p-1 key -> p-1's account", "acct-p-1", true},
		{"p-1 key -> its team's account", "acct-team-1", false},
		{"p-1 key -> another team's account", "acct-team-2", false},
		{"p-1 key -> a system account", "acct-shared", false},
		{"p-1 key -> another project's account", "acct-p-2", false},
	} {
		err := c.HostKey(ctx, projectKey(tc.account))
		if (err == nil) != tc.ok {
			t.Errorf("%s: err=%v, want ok=%v", tc.name, err, tc.ok)
		}
		if err != nil && status(err) != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", tc.name, status(err))
		}
	}
	teamKey := &hostkey.HostKey{Meta: meta.Metadata{Name: "k", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"}}, Spec: hostkey.Spec{HostID: "h-1", AccountID: "acct-shared"}}
	if err := c.HostKey(ctx, teamKey); err == nil {
		t.Error("t-1 key joined a system account")
	}
	if got := status(c.AccountRef(ctx, "acct-team-1", meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"})); got != 0 {
		t.Errorf("t-1 key -> t-1's account: status %d, want accepted", got)
	}
}

// Policy-binding accounts must exist and be visible to the caller; one the caller may not see answers as absent.
func TestPolicyBindingAccountsMustExist(t *testing.T) {
	rows := newLinkRows()
	ctx := actor.WithActor(context.Background(), &actor.Actor{UserID: "u-1"})
	c := rows.checker(hidingAuthz{hidden: map[string]bool{"acct-p-2": true}})
	// System-owned, so every account is in scope and only existence and visibility decide.
	bind := func(accounts ...string) *policybinding.PolicyBinding {
		return &policybinding.PolicyBinding{Meta: meta.Metadata{Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: policybinding.Spec{AccountIDs: accounts}}
	}
	if err := c.PolicyBinding(ctx, bind("acct-shared", "acct-p-1")); err != nil {
		t.Fatalf("existing accounts: %v", err)
	}
	for id, want := range map[string]int{"acct-gone": http.StatusBadRequest, "acct-p-2": http.StatusNotFound} {
		if got := status(c.PolicyBinding(ctx, bind(id))); got != want {
			t.Errorf("%s: status %d, want %d", id, got, want)
		}
	}
}

// A key's lane is an account its principal holds, or a provider account the caller may use; an empty one takes the principal's own account; a closed one is refused.
func TestKeyAccount(t *testing.T) {
	rows := newLinkRows()
	personal := account.Personal("u-1", "alice")
	personal.Meta.ID = "personal-u-1"
	other := account.Personal("u-2", "bob")
	other.Meta.ID = "personal-u-2"
	inProject := meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}
	closed := account.Member("u-1", "alice", inProject, "p-1", "")
	closed.Meta.ID, closed.Spec.Closed, closed.FollowsRoleBindings = "member-u-1", true, true
	granted := account.Member("u-1", "alice", meta.Owner{Kind: meta.OwnerProject, ID: "p-2"}, "p-2", "")
	granted.Meta.ID, granted.FollowsRoleBindings = "member-granted", true
	firstUse := account.Member("u-1", "alice", meta.Owner{Kind: meta.OwnerProject, ID: "p-3"}, "p-3", "")
	firstUse.Meta.ID = "member-first-use"
	ownOrg := &account.Account{Meta: meta.Metadata{ID: "own-org", Name: "alice-org", Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-1"}}, Spec: account.Spec{HostID: "h-1"}}
	for _, a := range []*account.Account{personal, other, closed, granted, firstUse, ownOrg} {
		rows.accounts[a.Meta.ID] = a
	}
	c := rows.checker(nil)
	c.Rows.DefaultAccount = func(_ context.Context, p key.Principal) (string, error) { return "personal-" + p.ID, nil }
	ctx := context.Background()
	keyIn := func(acct string) *key.Key {
		return &key.Key{Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalUser, ID: "u-1"}, AccountID: acct}}
	}
	for _, tc := range []struct {
		name, account string
		want          int
	}{
		{"own personal account", "personal-u-1", 0},
		{"a member account a role binding grants", "member-granted", 0},
		{"a provider account the user owns", "own-org", 0},
		{"a provider account the user does not own", "acct-shared", http.StatusForbidden},
		{"a member account opened on first use", "member-first-use", http.StatusForbidden},
		{"another user's account", "personal-u-2", http.StatusForbidden},
		{"a closed member account", "member-u-1", http.StatusBadRequest},
		{"an account that does not exist", "acct-gone", http.StatusBadRequest},
	} {
		if got := status(c.KeyAccount(ctx, keyIn(tc.account))); got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
	k := keyIn("")
	if err := c.KeyAccount(ctx, k); err != nil || k.Spec.AccountID != "personal-u-1" {
		t.Fatalf("defaulted lane = %q, %v; want the personal account", k.Spec.AccountID, err)
	}
}
