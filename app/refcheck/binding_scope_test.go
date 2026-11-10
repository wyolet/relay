package refcheck

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
)

// scopeChecker is the link rows with a policy per owner, the caller accounts of t-1, p-1, p-2, a service account of p-1 and a member of p-1 ("u-member") who may manage p-1's policy bindings, plus that member's personal account; t-1 holds p-1, t-2 holds p-2.
func scopeChecker() Checker {
	rows := newLinkRows()
	for _, a := range []*account.Account{
		account.ForTeam("t-1", "t-1", ""), account.ForProject("p-1", "p-1", ""), account.ForProject("p-2", "p-2", ""),
		account.ForServiceAccount("sa-p-1", "sa-p-1", "", "p-1"), account.Personal("u-member", "u-member"),
		account.Member("u-member", "u-member", meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}, "p-1", ""),
	} {
		a.Meta.ID = "caller-" + a.Meta.Name
		rows.accounts[a.Meta.ID] = a
	}
	c := rows.checker(nil)
	pols := map[string]*policy.Policy{}
	for id, o := range map[string]meta.Owner{
		"pol-system": {Kind: meta.OwnerSystem},
		"pol-host":   {Kind: meta.OwnerHost, ID: "h-1"},
		"pol-t-1":    {Kind: meta.OwnerTeam, ID: "t-1"},
		"pol-t-2":    {Kind: meta.OwnerTeam, ID: "t-2"},
		"pol-p-1":    {Kind: meta.OwnerProject, ID: "p-1"},
		"pol-p-2":    {Kind: meta.OwnerProject, ID: "p-2"},
		"pol-user":   {Kind: meta.OwnerUser, ID: "u-member"},
		"pol-other":  {Kind: meta.OwnerUser, ID: "u-stranger"},
	} {
		pols[id] = &policy.Policy{Meta: meta.Metadata{ID: id, Name: id, Owner: o}}
	}
	c.Rows.Policy = func(_ context.Context, id string) *policy.Policy { return pols[id] }
	c.Rows.UserMayBindIn = func(_ context.Context, userID string, scope meta.Owner) (bool, error) {
		return userID == "u-member" && scope == meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}, nil
	}
	return c
}

func scopedBinding(owner meta.Owner, policyID string, accounts ...string) *policybinding.PolicyBinding {
	return &policybinding.PolicyBinding{Meta: meta.Metadata{Name: "b", Owner: owner}, Spec: policybinding.Spec{PolicyID: policyID, AccountIDs: accounts}}
}

// A binding binds only accounts inside its owner's scope: a project its own account and the accounts it owns; a team the same for itself and its projects; a user the accounts the user owns; the system any.
func TestPolicyBindingAccountsStayInsideTheOwner(t *testing.T) {
	c := scopeChecker()
	ctx := context.Background()
	sys := meta.Owner{Kind: meta.OwnerSystem}
	t1 := meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"}
	p1 := meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}
	self := meta.Owner{Kind: meta.OwnerUser, ID: "u-member"}
	for _, tc := range []struct {
		name    string
		owner   meta.Owner
		account string
		want    int // 0 = accepted
	}{
		{"project binds its own account", p1, "caller-project-p-1", 0},
		{"project binds its service account's account", p1, "caller-sa-sa-p-1", 0},
		{"project binds a member's account", p1, "caller-u-member-project-p-1", 0},
		{"project binds its provider account", p1, "acct-p-1", 0},
		{"project binds another project's account", p1, "caller-project-p-2", http.StatusBadRequest},
		{"project binds its team's account", p1, "caller-team-t-1", http.StatusBadRequest},
		{"project binds a member's personal account", p1, "caller-user-u-member", http.StatusBadRequest},
		{"team binds its own account", t1, "caller-team-t-1", 0},
		{"team binds its project's account", t1, "caller-project-p-1", 0},
		{"team binds its project's provider account", t1, "acct-p-1", 0},
		{"team binds another team's project's account", t1, "caller-project-p-2", http.StatusBadRequest},
		{"team binds a shared account", t1, "acct-shared", http.StatusBadRequest},
		{"user binds their personal account", self, "caller-user-u-member", 0},
		{"user binds their member account", self, "caller-u-member-project-p-1", http.StatusBadRequest},
		{"system binds a personal account", sys, "caller-user-u-member", 0},
		{"system binds a shared account", sys, "acct-shared", 0},
		{"an account that does not exist", sys, "acct-gone", http.StatusBadRequest},
	} {
		if got := status(c.PolicyBinding(ctx, scopedBinding(tc.owner, "pol-system", tc.account))); got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
}

// A binding uses a policy of its own scope, an ancestor of it, the system, or a host; a sibling's or a descendant's is refused. A person's policy is bound by that person, or by a team or project where that person may manage policy bindings.
func TestPolicyBindingPolicyComesFromItsScopeOrAbove(t *testing.T) {
	c := scopeChecker()
	ctx := context.Background()
	project, team, personal := "caller-project-p-1", "caller-team-t-1", "caller-user-u-member"
	ownedByMember := meta.Owner{Kind: meta.OwnerUser, ID: "u-member"}
	for _, tc := range []struct {
		name    string
		owner   meta.Owner
		account string
		policy  string
		ok      bool
	}{
		{"team binding, its own policy", meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"}, team, "pol-t-1", true},
		{"team binding, a sibling team's policy", meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"}, team, "pol-t-2", false},
		{"team binding, its project's policy", meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"}, team, "pol-p-1", false},
		{"team binding, a system policy", meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"}, team, "pol-system", true},
		{"team binding, a host tier preset", meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"}, team, "pol-host", true},
		{"project binding, its team's policy", meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}, project, "pol-t-1", true},
		{"project binding, another team's policy", meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}, project, "pol-t-2", false},
		{"project binding, another project's policy", meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}, project, "pol-p-2", false},
		{"project binding, a personal policy of someone binding there", meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}, project, "pol-user", true},
		{"project binding, a personal policy of someone who may not bind there", meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}, project, "pol-other", false},
		{"team binding, a personal policy of someone binding only in its project", meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"}, team, "pol-user", false},
		{"user's own binding, their own policy", ownedByMember, personal, "pol-user", true},
		{"user's own binding, another user's policy", ownedByMember, personal, "pol-other", false},
		{"user's own binding, a system policy", ownedByMember, personal, "pol-system", true},
		{"user's own binding, a project's policy", ownedByMember, personal, "pol-p-1", false},
		{"system binding, a personal policy", meta.Owner{Kind: meta.OwnerSystem}, personal, "pol-user", false},
		{"system binding, a team's policy", meta.Owner{Kind: meta.OwnerSystem}, project, "pol-t-1", false},
		{"system binding, a host tier preset", meta.Owner{Kind: meta.OwnerSystem}, project, "pol-host", true},
	} {
		err := c.PolicyBinding(ctx, scopedBinding(tc.owner, tc.policy, tc.account))
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok %v", tc.name, err, tc.ok)
		}
		if err != nil && status(err) != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", tc.name, status(err))
		}
	}
}

// An account is held by one binding: a second binding naming it answers 409 duplicate_binding.
func TestPolicyBindingAccountHeldOnce(t *testing.T) {
	c := scopeChecker()
	ctx := context.Background()
	sys := meta.Owner{Kind: meta.OwnerSystem}
	held := scopedBinding(sys, "pol-system", "caller-project-p-1")
	held.Meta.ID = "held"
	c.Rows.PolicyBindings = func(context.Context) ([]*policybinding.PolicyBinding, error) {
		return []*policybinding.PolicyBinding{held}, nil
	}
	err := c.PolicyBinding(ctx, scopedBinding(sys, "pol-system", "caller-team-t-1", "caller-project-p-1"))
	var re *Error
	if status(err) != http.StatusConflict || !errors.As(err, &re) || re.Code != CodeDuplicateBinding {
		t.Fatalf("second binding of one account: %v, want 409 duplicate_binding", err)
	}
	if err := c.PolicyBinding(ctx, held); err != nil {
		t.Fatalf("re-saving the holding binding: %v", err)
	}
}

// linkingChecker is scopeChecker with system policies that link pools, and a system pool holding p-1's key.
func linkingChecker() Checker {
	c := scopeChecker()
	sys := meta.Owner{Kind: meta.OwnerSystem}
	linking := map[string]policy.Spec{
		"links-system-pool":  {PoolIDs: []string{"pool-shared"}},
		"links-team-1-pool":  {PoolIDs: []string{"pool-team-1"}},
		"links-team-2-pool":  {PoolIDs: []string{"pool-team-2"}},
		"links-p-1-pool":     {PoolIDs: []string{"pool-p-1"}},
		"links-pool-holding": {PoolIDs: []string{"pool-holding"}},
	}
	holding := &pool.Pool{Meta: meta.Metadata{ID: "pool-holding", Name: "pool-holding", Owner: sys}, Spec: pool.Spec{HostID: "h-1", CredentialIDs: []string{"key-project"}}}
	policies, pools := c.Rows.Policy, c.Rows.Pool
	c.Rows.Policy = func(ctx context.Context, id string) *policy.Policy {
		if s, ok := linking[id]; ok {
			return &policy.Policy{Meta: meta.Metadata{ID: id, Name: id, Owner: sys}, Spec: s}
		}
		return policies(ctx, id)
	}
	c.Rows.Pool = func(ctx context.Context, id string) *pool.Pool {
		if id == holding.Meta.ID {
			return holding
		}
		return pools(ctx, id)
	}
	return c
}

// A binding uses a policy only when its owner's scope may use every pool the policy links and every credential inside them: a shared policy never hands one scope another scope's credentials.
func TestPolicyBindingPolicyLinksStayInsideTheOwner(t *testing.T) {
	c := linkingChecker()
	ctx := context.Background()
	sys := meta.Owner{Kind: meta.OwnerSystem}
	p1, p2 := meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}, meta.Owner{Kind: meta.OwnerProject, ID: "p-2"}
	t2 := meta.Owner{Kind: meta.OwnerTeam, ID: "t-2"}
	acctP1, acctP2 := "caller-project-p-1", "caller-project-p-2"
	for _, tc := range []struct {
		name    string
		owner   meta.Owner
		account string
		policy  string
		ok      bool
	}{
		{"project, a system pool", p1, acctP1, "links-system-pool", true},
		{"project, its team's pool", p1, acctP1, "links-team-1-pool", true},
		{"project, another team's pool", p1, acctP1, "links-team-2-pool", false},
		{"project, its own pool", p1, acctP1, "links-p-1-pool", true},
		{"project, another project's pool", p2, acctP2, "links-p-1-pool", false},
		{"team, a project pool of another team", t2, acctP2, "links-p-1-pool", false},
		{"project, a system pool holding another project's key", p2, acctP2, "links-pool-holding", false},
		{"project, a system pool holding its own key", p1, acctP1, "links-pool-holding", true},
		{"system, a project's pool", sys, acctP1, "links-p-1-pool", false},
		{"system, a system pool", sys, acctP2, "links-system-pool", true},
	} {
		err := c.PolicyBinding(ctx, scopedBinding(tc.owner, tc.policy, tc.account))
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok %v", tc.name, err, tc.ok)
		}
		if err != nil && status(err) != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", tc.name, status(err))
		}
	}
}

// Editing a bound policy to link a pool its binding's scope may not use is refused, as binding it would be.
func TestBoundPolicyCannotGainAnUnusableLink(t *testing.T) {
	c := linkingChecker()
	ctx := context.Background()
	p2 := meta.Owner{Kind: meta.OwnerProject, ID: "p-2"}
	c.Rows.PolicyBindingsFor = func(_ context.Context, id string) ([]*policybinding.PolicyBinding, error) {
		return []*policybinding.PolicyBinding{scopedBinding(p2, id, "caller-project-p-2")}, nil
	}
	edited := c.Rows.Policy(ctx, "links-system-pool")
	if err := c.Policy(ctx, edited); err != nil {
		t.Fatalf("bound policy on a system pool refused: %v", err)
	}
	edited.Spec.PoolIDs = []string{"pool-team-1"}
	if err := c.Policy(ctx, edited); status(err) != http.StatusBadRequest {
		t.Fatalf("bound policy gaining another team's pool: err = %v, want 400", err)
	}
}
