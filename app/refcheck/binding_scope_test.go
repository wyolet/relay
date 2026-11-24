package refcheck

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/meta"
)

var member = meta.Owner{Kind: meta.OwnerUser, ID: "u-member"}

// scopeChecker is the link rows with a policy per owner, the caller accounts of t-1, p-1, p-2, a service account of p-1 and a member of p-1 ("u-member") who may manage p-1's policy bindings, plus that member's personal account; t-1 holds p-1, t-2 holds p-2. The policies named links-* are the system's and link the pool their name says; pool-holding is a system pool holding p-1's key, pool-drawing one drawing on p-1's account.
func scopeChecker(a authz.Authorizer) Checker {
	rows := newLinkRows()
	for _, a := range []*account.Account{
		account.ForTeam("t-1", "t-1", ""), account.ForProject("p-1", "p-1", ""), account.ForProject("p-2", "p-2", ""),
		account.ForServiceAccount("sa-p-1", "sa-p-1", "", projectOne), account.Personal("u-member", "u-member"),
		account.Member("u-member", "u-member", projectOne, "p-1", ""),
	} {
		a.Meta.ID = "caller-" + a.Meta.Name
		rows.accounts[a.Meta.ID] = a
	}
	rows.pools["pool-holding"] = &pool.Pool{Meta: meta.Metadata{ID: "pool-holding", Name: "pool-holding", Owner: system}, Spec: pool.Spec{HostID: "h-1", CredentialIDs: []string{"key-gone", "key-project"}}}
	rows.pools["pool-drawing"] = &pool.Pool{Meta: meta.Metadata{ID: "pool-drawing", Name: "pool-drawing", Owner: system}, Spec: pool.Spec{HostID: "h-1", AccountIDs: []string{"acct-gone", "acct-p-1"}}}
	c := rows.checker(a)
	pols := map[string]*policy.Policy{}
	for id, o := range map[string]meta.Owner{
		"pol-system": system,
		"pol-host":   {Kind: meta.OwnerHost, ID: "h-1"},
		"pol-t-1":    teamOne,
		"pol-t-2":    teamTwo,
		"pol-p-1":    projectOne,
		"pol-p-2":    projectTwo,
		"pol-user":   member,
		"pol-other":  {Kind: meta.OwnerUser, ID: "u-stranger"},
		"pol-broken": {Kind: meta.OwnerUser, ID: "u-broken"},
	} {
		pols[id] = &policy.Policy{Meta: meta.Metadata{ID: id, Name: id, Owner: o}}
	}
	for id, poolID := range map[string]string{
		"links-system-pool": "pool-shared", "links-team-1-pool": "pool-team-1", "links-team-2-pool": "pool-team-2", "links-p-1-pool": "pool-p-1",
		"links-pool-holding": "pool-holding", "links-pool-drawing": "pool-drawing", "links-pool-gone": "pool-gone",
	} {
		pols[id] = &policy.Policy{Meta: meta.Metadata{ID: id, Name: id, Owner: system}, Spec: policy.Spec{PoolIDs: []string{poolID}}}
	}
	c.Rows.Policy = func(_ context.Context, id string) *policy.Policy { return pols[id] }
	c.Rows.UserMayBindIn = func(_ context.Context, userID string, scope meta.Owner) (bool, error) {
		if userID == "u-broken" {
			return false, errors.New("store down")
		}
		return userID == "u-member" && scope == projectOne, nil
	}
	return c
}

func scopedBinding(owner meta.Owner, policyID string, accounts ...string) *policybinding.PolicyBinding {
	return &policybinding.PolicyBinding{Meta: meta.Metadata{Name: "b", Owner: owner}, Spec: policybinding.Spec{PolicyID: policyID, AccountIDs: accounts}}
}

// A binding's owning scope must exist, and it binds only accounts inside it: a project its own account and the accounts it owns; a team the same for itself and its projects; a user the accounts the user owns; the system any.
func TestPolicyBindingAccountsStayInsideTheOwner(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		authz    authz.Authorizer
		owner    meta.Owner
		accounts []string
		want     string
	}{
		{"project binds its own accounts", nil, projectOne, []string{"caller-project-p-1", "caller-sa-sa-p-1", "caller-u-member-project-p-1", "acct-p-1"}, ""},
		{"project binds another project's account", nil, projectOne, []string{"acct-p-1", "caller-project-p-2"}, `400: accountIds[1]: account "p-2" of project "p-2" is outside the scope of this binding's owner, project "p-1": a project's binding binds the project's own accounts: its account, its members', its service accounts' and its provider accounts. Create the binding in project "p-2" instead.`},
		{"project binds its team's account", nil, projectOne, []string{"caller-team-t-1"}, `400: accountIds[0]: account "t-1" of team "t-1" is outside the scope of this binding's owner, project "p-1": a project's binding binds the project's own accounts: its account, its members', its service accounts' and its provider accounts. Create the binding in team "t-1" instead.`},
		{"project binds a member's personal account", nil, projectOne, []string{"caller-user-u-member"}, `400: accountIds[0]: another user's personal account "u-member" is outside the scope of this binding's owner, project "p-1": a project's binding binds the project's own accounts: its account, its members', its service accounts' and its provider accounts. Create the binding in another user's personal scope instead.`},
		{"team binds its own and its project's accounts", nil, teamOne, []string{"caller-team-t-1", "caller-project-p-1", "acct-p-1"}, ""},
		{"team binds another team's project's account", nil, teamOne, []string{"caller-project-p-2"}, `400: accountIds[0]: account "p-2" of project "p-2" is outside the scope of this binding's owner, team "t-1": a team's binding binds the accounts of the team and its projects. Create the binding in project "p-2" instead.`},
		{"team binds a system account", nil, teamOne, []string{"acct-shared"}, `400: accountIds[0]: system account "acct-shared" is outside the scope of this binding's owner, team "t-1": a team's binding binds the accounts of the team and its projects. Create the binding in the system instead.`},
		{"user binds their personal account", nil, member, []string{"caller-user-u-member"}, ""},
		{"user binds their member account", nil, member, []string{"caller-u-member-project-p-1"}, `400: accountIds[0]: account "u-member in p-1" of project "p-1" is outside the scope of this binding's owner, another user's personal scope: a personal binding binds only its owner's own accounts. Create the binding in project "p-1" instead.`},
		{"system binds any account", nil, system, []string{"caller-user-u-member", "acct-shared", "acct-p-1"}, ""},
		{"an account that does not exist", nil, system, []string{"acct-shared", "acct-gone"}, `400: accountIds[1]: account "acct-gone" does not exist`},
		{"an account the caller cannot see", hiding("acct-p-2"), system, []string{"acct-p-2"}, `404: account "acct-p-2" not found`},
		{"a project that does not exist", nil, meta.Owner{Kind: meta.OwnerProject, ID: "p-9"}, []string{"acct-shared"}, `400: project "p-9" does not exist`},
		{"a team the caller cannot see", hiding("t-1"), teamOne, []string{"caller-team-t-1"}, `404: team "t-1" not found`},
	} {
		wantRefused(t, tc.name, scopeChecker(tc.authz).PolicyBinding(ctx, scopedBinding(tc.owner, "pol-system", tc.accounts...)), tc.want)
	}
}

// A binding uses a policy of its own scope, an ancestor of it, the system, or a host; a sibling's or a descendant's is refused. A person's policy is bound by that person, or by a team or project where that person may manage policy bindings.
func TestPolicyBindingPolicyComesFromItsScopeOrAbove(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		authz  authz.Authorizer
		owner  meta.Owner
		policy string
		want   string
	}{
		{"team, its own policy", nil, teamOne, "pol-t-1", ""},
		{"team, a sibling team's policy", nil, teamOne, "pol-t-2", `400: a policy binding of team "t-1" cannot use policy "pol-t-2" of team "t-2": a team's binding can use policies of the team or the system, a host's tier policies, or a personal policy of someone who may manage policy bindings there. Create the policy in the team, or move this one there.`},
		{"team, its project's policy", nil, teamOne, "pol-p-1", `400: a policy binding of team "t-1" cannot use policy "pol-p-1" of project "p-1": a team's binding can use policies of the team or the system, a host's tier policies, or a personal policy of someone who may manage policy bindings there. Create the policy in the team, or move this one there.`},
		{"team, a system policy", nil, teamOne, "pol-system", ""},
		{"team, a host tier preset", nil, teamOne, "pol-host", ""},
		{"project, its team's policy", nil, projectOne, "pol-t-1", ""},
		{"project, another team's policy", nil, projectOne, "pol-t-2", `400: a policy binding of project "p-1" cannot use policy "pol-t-2" of team "t-2": a project's binding can use policies of the project, its team or the system, a host's tier policies, or a personal policy of someone who may manage policy bindings there. Create the policy in the project or its team, or move this one there.`},
		{"project, another project's policy", nil, projectOne, "pol-p-2", `400: a policy binding of project "p-1" cannot use policy "pol-p-2" of project "p-2": a project's binding can use policies of the project, its team or the system, a host's tier policies, or a personal policy of someone who may manage policy bindings there. Create the policy in the project or its team, or move this one there.`},
		{"project, a personal policy of someone binding there", nil, projectOne, "pol-user", ""},
		{"project, a personal policy of someone who may not bind there", nil, projectOne, "pol-other", `400: another user's personal policy "pol-other" cannot be bound in project "p-1": its owner may not manage policy bindings there. Use a policy of the project, its team or the system instead.`},
		{"project, a personal policy whose owner's permissions cannot be read", nil, projectOne, "pol-broken", "500: store down"},
		{"team, a personal policy of someone binding only in its project", nil, teamOne, "pol-user", `400: another user's personal policy "pol-user" cannot be bound in team "t-1": its owner may not manage policy bindings there. Use a policy of the team or the system instead.`},
		{"user, their own policy", nil, member, "pol-user", ""},
		{"user, another user's policy", nil, member, "pol-other", `400: a policy binding of another user's personal scope cannot use another user's personal policy "pol-other": a personal policy is bound by its owner to their own accounts, or in a team or project where its owner may manage policy bindings. Use a policy of the same user or the system instead.`},
		{"user, a system policy", nil, member, "pol-system", ""},
		{"user, a project's policy", nil, member, "pol-p-1", `400: a policy binding of another user's personal scope cannot use policy "pol-p-1" of project "p-1": a personal binding can use policies of the same user or the system, a host's tier policies, or a personal policy of someone who may manage policy bindings there. Create the policy in the same personal scope, or move this one there.`},
		{"system, a personal policy", nil, system, "pol-user", `400: a policy binding of the system cannot use another user's personal policy "pol-user": a personal policy is bound by its owner to their own accounts, or in a team or project where its owner may manage policy bindings. Use a policy of the system instead.`},
		{"system, a team's policy", nil, system, "pol-t-1", `400: a policy binding of the system cannot use policy "pol-t-1" of team "t-1": a system binding can use policies of the system, a host's tier policies, or a personal policy of someone who may manage policy bindings there. Create the policy in the system, or move this one there.`},
		{"system, a host tier preset", nil, system, "pol-host", ""},
		{"a policy that does not exist", nil, system, "pol-gone", `400: policy "pol-gone" does not exist`},
		{"a policy the caller cannot see", hiding("pol-system"), system, "pol-system", `404: policy "pol-system" not found`},
		{"no policy", nil, system, "", ""},
	} {
		wantRefused(t, tc.name, scopeChecker(tc.authz).PolicyBinding(ctx, scopedBinding(tc.owner, tc.policy)), tc.want)
	}
	// Without a permission lookup (a loader) a team or project binds any personal policy; the plans re-check.
	loader := scopeChecker(nil)
	loader.Rows.UserMayBindIn = nil
	wantRefused(t, "loader, a stranger's personal policy in a project", loader.PolicyBinding(ctx, scopedBinding(projectOne, "pol-other")), "")
}

// A binding uses a policy only when its owner's scope may use every pool the policy links and every credential and account inside them: a shared policy never hands one scope another scope's credentials.
func TestPolicyBindingPolicyLinksStayInsideTheOwner(t *testing.T) {
	c := scopeChecker(nil)
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		owner  meta.Owner
		policy string
		want   string
	}{
		{"project, a system pool", projectOne, "links-system-pool", ""},
		{"project, its team's pool", projectOne, "links-team-1-pool", ""},
		{"project, another team's pool", projectOne, "links-team-2-pool", `400: policy "links-team-2-pool" uses pool "pool-team-2" of team "t-2"; a policy binding of project "p-1" can use pools of the project, its team or the system. Create the pool in the project or its team, or move this one there.`},
		{"project, its own pool", projectOne, "links-p-1-pool", ""},
		{"project, another project's pool", projectTwo, "links-p-1-pool", `400: policy "links-p-1-pool" uses pool "pool-p-1" of project "p-1"; a policy binding of project "p-2" can use pools of the project, its team or the system. Create the pool in the project or its team, or move this one there.`},
		{"team, a project pool of another team", teamTwo, "links-p-1-pool", `400: policy "links-p-1-pool" uses pool "pool-p-1" of project "p-1"; a policy binding of team "t-2" can use pools of the team or the system. Create the pool in the team, or move this one there.`},
		{"project, a system pool holding another project's key", projectTwo, "links-pool-holding", `400: policy "links-pool-holding" uses system pool "pool-holding", which holds host key "key-project" of project "p-1"; a policy binding of project "p-2" can use host keys of the project, its team or the system. Take the key out of the pool, or move it to the project or its team.`},
		{"project, a system pool holding its own key", projectOne, "links-pool-holding", ""},
		{"project, a system pool drawing on another project's account", projectTwo, "links-pool-drawing", `400: policy "links-pool-drawing" uses system pool "pool-drawing", which draws on account "acct-p-1" of project "p-1"; a policy binding of project "p-2" can use accounts of the project, its team or the system. Take the account out of the pool, or share it with project "p-2".`},
		{"project, a system pool drawing on its own account", projectOne, "links-pool-drawing", ""},
		{"system, a project's pool", system, "links-p-1-pool", `400: policy "links-p-1-pool" uses pool "pool-p-1" of project "p-1"; a policy binding of the system can use pools of the system. Create the pool in the system, or move this one there.`},
		{"system, a system pool", system, "links-system-pool", ""},
		{"a pool that does not exist is left to the plan", projectOne, "links-pool-gone", ""},
	} {
		wantRefused(t, tc.name, c.PolicyBinding(ctx, scopedBinding(tc.owner, tc.policy)), tc.want)
	}
}

// Editing a bound policy to link a pool its binding's scope may not use is refused, as binding it would be.
func TestBoundPolicyCannotGainAnUnusableLink(t *testing.T) {
	c := scopeChecker(nil)
	ctx := context.Background()
	var asked []string
	var lookupErr error
	c.Rows.PolicyBindingsFor = func(_ context.Context, id string) ([]*policybinding.PolicyBinding, error) {
		asked = append(asked, id)
		return []*policybinding.PolicyBinding{scopedBinding(projectTwo, id)}, lookupErr
	}
	edited := *c.Rows.Policy(ctx, "links-system-pool")
	wantRefused(t, "bound policy on a system pool", c.Policy(ctx, &edited), "")
	edited.Spec.PoolIDs = []string{"pool-shared", "pool-team-1"}
	wantRefused(t, "bound policy gaining another team's pool", c.Policy(ctx, &edited), `400: policy "links-system-pool" uses pool "pool-team-1" of team "t-1"; a policy binding of project "p-2" can use pools of the project, its team or the system. Create the pool in the project or its team, or move this one there.`)
	lookupErr = errors.New("store down")
	wantRefused(t, "the binding lookup failing", c.Policy(ctx, &edited), "500: store down")
	if len(asked) != 3 || asked[0] != "links-system-pool" {
		t.Errorf("bindings read for %v, want links-system-pool three times", asked)
	}
}

// An account is held by one binding: a second binding naming it answers 409 duplicate_binding, naming the holder only to a caller who sees it.
func TestPolicyBindingAccountHeldOnce(t *testing.T) {
	ctx := context.Background()
	held := scopedBinding(system, "pol-system", "caller-project-p-1")
	held.Meta.ID, held.Meta.DisplayName = "held", "Held"
	second := scopedBinding(system, "pol-system", "caller-team-t-1", "caller-project-p-1")
	var lookupErr error
	for _, tc := range []struct {
		name  string
		authz authz.Authorizer
		pb    *policybinding.PolicyBinding
		want  string
	}{
		{"a second binding of one account", nil, second, `409 duplicate_binding: account "p-1" of project "p-1" is already bound by policy binding "Held": an account is bound to one policy. Remove it from that binding first, or edit that binding instead.`},
		{"the holder hidden from the caller", hiding("held"), second, `409 duplicate_binding: account "p-1" of project "p-1" is already bound by another policy binding: an account is bound to one policy. Remove it from that binding first, or edit that binding instead.`},
		{"re-saving the holding binding", nil, held, ""},
		{"a binding of other accounts", nil, scopedBinding(system, "pol-system", "caller-team-t-1"), ""},
	} {
		c := scopeChecker(tc.authz)
		c.Rows.PolicyBindings = func(context.Context) ([]*policybinding.PolicyBinding, error) {
			return []*policybinding.PolicyBinding{held}, lookupErr
		}
		wantRefused(t, tc.name, c.PolicyBinding(ctx, tc.pb), tc.want)
		if tc.pb == second && tc.authz == nil {
			lookupErr = errors.New("store down")
			wantRefused(t, "the binding list failing", c.PolicyBinding(ctx, tc.pb), "500: store down")
			lookupErr = nil
		}
	}
}
