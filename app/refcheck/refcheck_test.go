package refcheck

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
)

func adminRole(userID string) context.Context {
	return actor.WithActor(context.Background(), &actor.Actor{UserID: userID, Roles: []string{user.RoleAdmin}})
}

func adminToken() context.Context {
	return actor.WithActor(context.Background(), &actor.Actor{AdminToken: true})
}

func TestRequireAdmin(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ctx   context.Context
		authz authz.Authorizer
		want  string
	}{
		{"single-user mode, anyone", as("u-1"), nil, ""},
		{"scoped, the admin role", adminRole("u-1"), hiding(), ""},
		{"scoped, the admin token", adminToken(), hiding(), ""},
		{"scoped, a member", as("u-1"), hiding(), "403: authz: forbidden: the thing requires an admin"},
		{"scoped, no caller", context.Background(), hiding(), "403: authz: forbidden: the thing requires an admin"},
	} {
		err := RequireAdmin(tc.ctx, tc.authz, "the thing")
		wantRefused(t, tc.name, err, tc.want)
		if err != nil && !errors.Is(err, authz.ErrForbidden) {
			t.Errorf("%s: err = %v, want it to wrap authz.ErrForbidden", tc.name, err)
		}
	}
}

func TestStampOwnerID(t *testing.T) {
	userOwner := func(id string) meta.Owner { return meta.Owner{Kind: meta.OwnerUser, ID: id} }
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		in      meta.Owner
		want    meta.Owner
		wantErr string
	}{
		{"not user-owned", as("u-1"), teamOne, teamOne, ""},
		{"empty id takes the caller", as("u-1"), userOwner(""), userOwner("u-1"), ""},
		{"empty id under the admin token is the system's", adminToken(), userOwner(""), system, ""},
		{"empty id with no caller is the system's", context.Background(), userOwner(""), system, ""},
		{"the caller's own id", as("u-1"), userOwner("u-1"), userOwner("u-1"), ""},
		{"someone else's id", as("u-1"), userOwner("u-2"), userOwner("u-2"), "owner.id must be empty or match the calling user"},
		{"someone else's id, by the admin role", adminRole("u-1"), userOwner("u-2"), userOwner("u-2"), "owner.id must be empty or match the calling user"},
		{"someone else's id, by the admin token", adminToken(), userOwner("u-2"), userOwner("u-2"), ""},
		{"someone's id with no caller", context.Background(), userOwner("u-2"), userOwner("u-2"), ""},
	} {
		got := tc.in
		err := StampOwnerID(tc.ctx, &got)
		if got != tc.want || (err == nil) != (tc.wantErr == "") || (err != nil && err.Error() != tc.wantErr) {
			t.Errorf("%s: owner %+v, err %v; want %+v, err %q", tc.name, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestSharedOwner(t *testing.T) {
	const refusal = `400: host key "k" lives outside this project: a project's rows can use host keys of the project or the system. Create the host key in the project, or move this one there.`
	for _, tc := range []struct {
		name            string
		owner, refOwner meta.Owner
		want            string
	}{
		{"same project", projectOne, projectOne, ""},
		{"system row", system, projectOne, ""},
		{"organization row", meta.Owner{Kind: meta.OwnerOrganization}, projectOne, ""},
		{"another project's row", projectTwo, projectOne, refusal},
		{"a team's row", teamOne, projectOne, refusal},
		{"a user's row", alice, projectOne, refusal},
		{"any row from a team", projectTwo, teamOne, ""},
		{"any row from the system", projectTwo, system, ""},
	} {
		wantRefused(t, tc.name, SharedOwner("host-key", "k", tc.owner, tc.refOwner), tc.want)
	}
}

// Catalog-owned rate limits and policies carry no credentials, so any scope may draw on them; a host's tier policy is never bound, and a tenant's rows stay inside the tenant.
func TestPolicyRefAndRateLimitRef(t *testing.T) {
	rls := map[string]*ratelimit.RateLimit{}
	pols := map[string]*policy.Policy{}
	for id, o := range map[string]meta.Owner{
		"system":        system,
		"provider":      {Kind: meta.OwnerProvider, ID: "pv-1"},
		"host":          {Kind: meta.OwnerHost, ID: "h-1"},
		"same-project":  projectOne,
		"other-project": projectTwo,
		"team":          teamOne,
		"organization":  {Kind: meta.OwnerOrganization},
		"hidden":        system,
	} {
		rls[id] = &ratelimit.RateLimit{Meta: meta.Metadata{ID: id, Name: id, Owner: o}}
		pols[id] = &policy.Policy{Meta: meta.Metadata{ID: id, Name: id, Owner: o}}
	}
	c := Checker{Authz: hiding("hidden"), Rows: Lookup{
		RateLimit: func(_ context.Context, id string) *ratelimit.RateLimit { return rls[id] },
		Policy:    func(_ context.Context, id string) *policy.Policy { return pols[id] },
		// p-1 sits in another team than t-1, so t-1's rows are not its own.
		Project: func(_ context.Context, id string) *project.Project {
			return &project.Project{Meta: meta.Metadata{ID: id, Name: id}, Spec: project.Spec{TeamID: "t-9"}}
		},
	}}
	ctx := as("u-1")
	scopes := map[string]meta.Owner{"user": {Kind: meta.OwnerUser, ID: "u-1"}, "team": teamOne, "project": projectOne}
	const hostTier = `400: policy "host" is a host tier policy and cannot be bound`
	for name, ref := range scopes {
		// The organization's rows serve every scope, as the system's do.
		for _, target := range []string{"system", "organization", "provider", "host", ""} {
			wantRefused(t, name+" policy, "+target+" rate limit", c.RateLimitRef(ctx, target, ref), "")
		}
		for _, target := range []string{"system", "organization", "provider", ""} {
			wantRefused(t, name+" row, "+target+" policy", c.PolicyRef(ctx, target, ref), "")
		}
		wantRefused(t, name+" row, a host tier policy", c.PolicyRef(ctx, "host", ref), hostTier)
	}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"rate limit of the same project", c.RateLimitRef(ctx, "same-project", projectOne), ""},
		{"rate limit of another project", c.RateLimitRef(ctx, "other-project", projectOne), `400: a policy of project "p-1" cannot use rate limit "other-project" of project "p-2": a project's policy can use rate limits of the project, its team or the system. Create the rate limit in the project or its team, or move this one there.`},
		{"rate limit of another team", c.RateLimitRef(ctx, "team", projectOne), `400: a policy of project "p-1" cannot use rate limit "team" of another team: a project's policy can use rate limits of the project, its team or the system. Create the rate limit in the project or its team, or move this one there.`},
		{"rate limit that does not exist", c.RateLimitRef(ctx, "gone", projectOne), `400: rate-limit "gone" does not exist`},
		{"rate limit the caller cannot see", c.RateLimitRef(ctx, "hidden", projectOne), `404: rate-limit "hidden" not found`},
		{"policy of the same project", c.PolicyRef(ctx, "same-project", projectOne), ""},
		{"policy of another project", c.PolicyRef(ctx, "other-project", projectOne), `400: a row of project "p-1" cannot use policy "other-project" of project "p-2": a project's rows can use policies of the project or the system. Create the policy in the project, or move this one there.`},
		{"policy of a team, from a project", c.PolicyRef(ctx, "team", projectOne), `400: a row of project "p-1" cannot use policy "team" of another team: a project's rows can use policies of the project or the system. Create the policy in the project, or move this one there.`},
		{"policy of a project, from a team", c.PolicyRef(ctx, "same-project", teamOne), ""},
		{"policy that does not exist", c.PolicyRef(ctx, "gone", projectOne), `400: policy "gone" does not exist`},
		{"policy the caller cannot see", c.PolicyRef(ctx, "hidden", projectOne), `404: policy "hidden" not found`},
	} {
		wantRefused(t, tc.name, tc.err, tc.want)
	}
}

// keyRows has service account sa-1 of project p-1 and users u-1 and u-2.
func keyRows(a authz.Authorizer) Checker {
	return Checker{Authz: a, Rows: Lookup{
		ServiceAccount: func(_ context.Context, id string) *serviceaccount.ServiceAccount {
			if id != "sa-1" {
				return nil
			}
			return &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: "sa-1", Name: "ci", Owner: projectOne}, Spec: serviceaccount.Spec{ProjectID: "p-1"}}
		},
		MissingUsers: func(_ context.Context, ids []string) ([]string, error) {
			var missing []string
			for _, id := range ids {
				if id == "u-broken" {
					return nil, errors.New("store down")
				}
				if id != "u-1" && id != "u-2" {
					missing = append(missing, id)
				}
			}
			return missing, nil
		},
	}}
}

// A key's owner follows its principal: a service account's project, or the user it names, who must be the caller unless the admin token writes it.
func TestKeyOwner(t *testing.T) {
	sa := func(id string) key.Principal { return key.Principal{Kind: key.PrincipalServiceAccount, ID: id} }
	usr := func(id string) key.Principal { return key.Principal{Kind: key.PrincipalUser, ID: id} }
	userOwner := func(id string) meta.Owner { return meta.Owner{Kind: meta.OwnerUser, ID: id} }
	for _, tc := range []struct {
		name      string
		ctx       context.Context
		c         Checker
		principal key.Principal
		wantOwner meta.Owner
		wantID    string
		want      string
	}{
		{"service account", as("u-1"), keyRows(nil), sa("sa-1"), projectOne, "sa-1", ""},
		{"service account that does not exist", as("u-1"), keyRows(nil), sa("sa-gone"), meta.Owner{}, "sa-gone", `404: service-account "sa-gone" not found`},
		{"service account the caller cannot see", as("u-1"), keyRows(hiding("sa-1")), sa("sa-1"), meta.Owner{}, "sa-1", `404: service-account "sa-1" not found`},
		{"service account with no lookup", as("u-1"), Checker{}, sa("sa-1"), meta.Owner{}, "sa-1", "400: service accounts are not available on this relay"},
		{"user, id left empty", as("u-1"), keyRows(nil), usr(""), userOwner("u-1"), "u-1", ""},
		{"user, the caller", as("u-1"), keyRows(nil), usr("u-1"), userOwner("u-1"), "u-1", ""},
		{"user, someone else", as("u-1"), keyRows(nil), usr("u-2"), userOwner("u-2"), "u-2", "400: owner.id must be empty or match the calling user"},
		{"user, someone else by the admin token", adminToken(), keyRows(nil), usr("u-2"), userOwner("u-2"), "u-2", ""},
		{"user who does not exist", adminToken(), keyRows(nil), usr("u-gone"), userOwner("u-gone"), "u-gone", `404: user "u-gone" not found`},
		{"unknown principal kind", as("u-1"), keyRows(nil), key.Principal{Kind: "group", ID: "g"}, meta.Owner{}, "g", "400: spec.principal.kind must be serviceaccount or user"},
	} {
		k := &key.Key{Spec: key.Spec{Principal: tc.principal, AccountID: "set"}}
		wantRefused(t, tc.name, tc.c.Key(tc.ctx, k), tc.want)
		if k.Meta.Owner != tc.wantOwner || k.Spec.Principal.ID != tc.wantID {
			t.Errorf("%s: owner %+v principal id %q, want %+v %q", tc.name, k.Meta.Owner, k.Spec.Principal.ID, tc.wantOwner, tc.wantID)
		}
	}
}

// Owners that mirror a spec field are re-derived from it, and the row it names must exist and be visible.
func TestOwnerFollowsSpec(t *testing.T) {
	rows := newLinkRows()
	ctx := as("u-1")
	stale := meta.Owner{Kind: meta.OwnerUser, ID: "u-1"}
	for _, tc := range []struct {
		name      string
		authz     authz.Authorizer
		id        string
		wantOwner meta.Owner
		want      string
	}{
		{"p-1", nil, "p-1", projectOne, ""},
		{"p-9", nil, "p-9", meta.Owner{Kind: meta.OwnerProject, ID: "p-9"}, `400: project "p-9" does not exist`},
		{"p-1 hidden", hiding("p-1"), "p-1", projectOne, `404: project "p-1" not found`},
	} {
		sa := &serviceaccount.ServiceAccount{Meta: meta.Metadata{Owner: stale}, Spec: serviceaccount.Spec{ProjectID: tc.id}}
		wantRefused(t, "service account in "+tc.name, rows.checker(tc.authz).ServiceAccount(ctx, sa), tc.want)
		if sa.Meta.Owner != tc.wantOwner {
			t.Errorf("service account in %s: owner %+v, want %+v", tc.name, sa.Meta.Owner, tc.wantOwner)
		}
	}
	for _, tc := range []struct {
		name      string
		authz     authz.Authorizer
		id        string
		wantOwner meta.Owner
		want      string
	}{
		{"t-1", nil, "t-1", teamOne, ""},
		{"t-9", nil, "t-9", meta.Owner{Kind: meta.OwnerTeam, ID: "t-9"}, `400: team "t-9" does not exist`},
		{"t-1 hidden", hiding("t-1"), "t-1", teamOne, `404: team "t-1" not found`},
	} {
		p := &project.Project{Meta: meta.Metadata{Owner: stale}, Spec: project.Spec{TeamID: tc.id}}
		wantRefused(t, "project in "+tc.name, rows.checker(tc.authz).Project(ctx, p), tc.want)
		if p.Meta.Owner != tc.wantOwner {
			t.Errorf("project in %s: owner %+v, want %+v", tc.name, p.Meta.Owner, tc.wantOwner)
		}
	}
}

func TestGroupMembersAreUsers(t *testing.T) {
	c := keyRows(nil)
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		members []string
		want    string
	}{
		{"no members", nil, ""},
		{"users", []string{"u-1", "u-2"}, ""},
		{"an id that is no user", []string{"u-1", "u-typo", "u-gone"}, `400: user "u-typo" does not exist`},
		{"the lookup failing", []string{"u-broken"}, "500: store down"},
	} {
		wantRefused(t, tc.name, c.Group(ctx, &group.Group{Spec: group.Spec{MemberIDs: tc.members}}), tc.want)
	}
	wantRefused(t, "no user lookup", Checker{}.Group(ctx, &group.Group{Spec: group.Spec{MemberIDs: []string{"u-gone"}}}), "")
}

// personalOffRows is bob's personal account, a provider account he owns and a member account of his, with bob's personal account turned off and alice's on.
func personalOffRows() Checker {
	personal := account.Personal("u-bob", "bob")
	personal.Meta.ID = "acct-bob"
	provider := &account.Account{Meta: meta.Metadata{ID: "acct-bob-org", Name: "bob-org", Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-bob"}}, Spec: account.Spec{HostID: "h-1"}}
	member := account.Member("u-bob", "bob", teamOne, "one", "")
	member.Meta.ID, member.FollowsRoleBindings = "acct-bob-member", true
	rows := map[string]*account.Account{personal.Meta.ID: personal, provider.Meta.ID: provider, member.Meta.ID: member}
	return Checker{Rows: Lookup{
		Account:        func(_ context.Context, id string) *account.Account { return rows[id] },
		DefaultAccount: func(context.Context, key.Principal) (string, error) { return "acct-bob", nil },
		PersonalAccountOff: func(_ context.Context, id string) (bool, string) {
			return id == "u-bob" || id == "u-nameless", map[string]string{"u-bob": "bob"}[id]
		},
	}}
}

// While a user's personal account is off, a key of theirs must name a lane other than it, their own provider accounts take no keys, and nothing is shared with them.
func TestPersonalAccountOff(t *testing.T) {
	c := personalOffRows()
	ctx := context.Background()
	keyIn := func(userID, accountID string) error {
		return c.KeyAccount(ctx, &key.Key{Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalUser, ID: userID}, AccountID: accountID}})
	}
	providerOf := func(o meta.Owner, hostID string) error {
		return c.PersonalProviderAccount(ctx, &account.Account{Meta: meta.Metadata{Owner: o}, Spec: account.Spec{HostID: hostID}})
	}
	bob := meta.Owner{Kind: meta.OwnerUser, ID: "u-bob"}
	src := &account.Account{Meta: meta.Metadata{ID: "acct-team", Name: "team-org", Owner: teamOne}, Spec: account.Spec{HostID: "h-1"}}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"key with no lane", keyIn("u-bob", ""), "400 lane_required: personal accounts are turned off for bob, so a key needs an account to run in: set accountId to a member account in one of their teams or projects"},
		{"key with no lane, user without a name", keyIn("u-nameless", ""), "400 lane_required: personal accounts are turned off for user u-nameless, so a key needs an account to run in: set accountId to a member account in one of their teams or projects"},
		{"key in the personal lane", keyIn("u-bob", "acct-bob"), `403 personal_account_off: personal accounts are turned off for bob, so a key cannot run in another user's personal account "bob"; use a team or project key`},
		{"key in a provider account he owns", keyIn("u-bob", "acct-bob-org"), `403 personal_account_off: personal accounts are turned off for bob, so a key cannot run in another user's personal account "bob-org"; use a team or project key`},
		{"key in a member lane", keyIn("u-bob", "acct-bob-member"), ""},
		{"alice's key with no lane", keyIn("u-alice", ""), ""},
		{"a new provider account of bob's", providerOf(bob, "h-1"), "403 personal_account_off: personal accounts are turned off for bob, so they cannot own a provider account; create it in a team or project"},
		{"a new provider account of alice's", providerOf(alice, "h-1"), ""},
		{"a new provider account of a team", providerOf(teamOne, "h-1"), ""},
		{"a caller account of bob's", providerOf(bob, ""), ""},
		{"a share with bob", c.newShare(ctx, src, account.Share(src, account.Receiver{Kind: account.ReceiverUser, ID: "u-bob"}, bob, "bob")), "403 personal_account_off: personal accounts are turned off for bob, so a share with them would serve nothing; share with their team or project instead"},
	} {
		wantRefused(t, tc.name, tc.err, tc.want)
	}
}

// payloadLogging decides whether a user's request bodies are kept, so the user it constrains never sets it.
func TestAccountPayloadLogging(t *testing.T) {
	personal := account.Personal("u-1", "alice")
	member := account.Member("u-1", "alice", projectOne, "p-1", "")
	teamAcct := account.ForTeam("t-1", "t-1", "")
	provider := &account.Account{Meta: meta.Metadata{Owner: alice}, Spec: account.Spec{HostID: "h-1"}}
	for _, tc := range []struct {
		name     string
		ctx      context.Context
		authz    authz.Authorizer
		existing *account.Account
		change   bool
		want     string
	}{
		{"personal, by its user", as("u-1"), hiding(), personal, true, "403: authz: forbidden: spec.payloadLogging of a personal account is set by an admin"},
		{"personal, unchanged", as("u-1"), hiding(), personal, false, ""},
		{"personal, by an admin", adminRole("u-9"), hiding(), personal, true, ""},
		{"personal, single-user mode", as("u-1"), nil, personal, true, ""},
		{"member, by its holder", as("u-1"), hiding(), member, true, "403: authz: forbidden: spec.payloadLogging of your own member account is set by an admin of its project"},
		{"member, by someone else in the scope", as("u-2"), hiding(), member, true, ""},
		{"team account", as("u-1"), hiding(), teamAcct, true, ""},
		{"provider account", as("u-1"), hiding(), provider, true, ""},
	} {
		incoming := *tc.existing
		if tc.change {
			incoming.Spec.PayloadLogging = account.PayloadLoggingOn
		}
		err := Checker{Authz: tc.authz}.AccountPayloadLogging(tc.ctx, tc.existing, &incoming)
		wantRefused(t, tc.name, err, tc.want)
		if err != nil && !errors.Is(err, authz.ErrForbidden) {
			t.Errorf("%s: err = %v, want it to wrap authz.ErrForbidden", tc.name, err)
		}
	}
}

type scopingAuthz struct{}

func (scopingAuthz) Authorize(context.Context, string, authz.Resource) error { return nil }

func (scopingAuthz) Visible(context.Context, string, string, meta.Owner) bool { return true }

// An env reference reads the relay's own process environment, so only an operator may write one; anyone else could read any variable through the key's health probe.
func TestEnvSourcedHostKeyNeedsAnAdmin(t *testing.T) {
	k := &hostkey.HostKey{
		Meta: meta.Metadata{Name: "k", Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-1"}},
		Spec: hostkey.Spec{HostID: "h-1", AccountID: "acct-1", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "HOME"}},
	}
	for _, tc := range []struct {
		name  string
		ctx   context.Context
		authz authz.Authorizer
		want  string
	}{
		{"a member", as("u-1"), scopingAuthz{}, "403: authz: forbidden: an env-sourced host key requires an admin"},
		{"an admin", adminRole("u-2"), scopingAuthz{}, ""},
		{"single-user mode", as("u-1"), nil, ""},
	} {
		wantRefused(t, tc.name, Checker{Authz: tc.authz}.HostKey(tc.ctx, k), tc.want)
	}
}

// teamGrant allows a fixed action set at one team, nothing elsewhere.
type teamGrant struct {
	teamID  string
	actions map[string]bool
}

func (g teamGrant) Authorize(_ context.Context, action string, res authz.Resource) error {
	if res.Owner != nil && res.Owner.Kind == meta.OwnerTeam && res.Owner.ID == g.teamID && g.actions[action] {
		return nil
	}
	return authz.ErrForbidden
}

// Binding a role hands out every permission in it, so the binder must hold each one at the scope; the role, the scope and the subjects must exist.
func TestRoleBinding(t *testing.T) {
	rows := newLinkRows()
	roles := map[string]*role.Role{
		"r-read":   {Meta: meta.Metadata{ID: "r-read", Name: "reader"}, Spec: role.Spec{Rules: []role.Rule{{Kinds: []string{"keys"}, Verbs: []string{"get"}}}}},
		"r-write":  {Meta: meta.Metadata{ID: "r-write", Name: "writer"}, Spec: role.Spec{Rules: []role.Rule{{Kinds: []string{"keys"}, Verbs: []string{"get", "delete"}}}}},
		"r-hidden": {Meta: meta.Metadata{ID: "r-hidden", Name: "hidden"}},
	}
	holdsRead := teamGrant{teamID: "t-1", actions: map[string]bool{"keys.get": true}}
	userSub := func(id string) rolebinding.Subject { return rolebinding.Subject{Kind: rolebinding.SubjectUser, ID: id} }
	saSub := func(id string) rolebinding.Subject {
		return rolebinding.Subject{Kind: rolebinding.SubjectServiceAccount, ID: id}
	}
	group := rolebinding.Subject{Kind: rolebinding.SubjectGroup, Name: "no-such-group"}
	for _, tc := range []struct {
		name     string
		ctx      context.Context
		authz    authz.Authorizer
		roleID   string
		scope    meta.Owner
		subjects []rolebinding.Subject
		want     string
	}{
		{"a role the binder holds", as("u-1"), holdsRead, "r-read", teamOne, []rolebinding.Subject{userSub("u-1"), saSub("sa-1"), group}, ""},
		{"a role wider than the binder holds", as("u-1"), holdsRead, "r-write", teamOne, nil, `403: authz: forbidden: binding role "writer" would grant keys.delete, which you do not hold at this scope`},
		{"a role held at another scope", as("u-1"), holdsRead, "r-read", teamTwo, nil, `403: authz: forbidden: binding role "reader" would grant keys.get, which you do not hold at this scope`},
		{"a wider role, by an admin", adminRole("u-1"), holdsRead, "r-write", teamTwo, nil, ""},
		{"a role that does not exist", as("u-1"), holdsRead, "r-gone", teamOne, nil, `400: role "r-gone" does not exist`},
		{"a role the binder cannot see", as("u-1"), hiding("r-hidden"), "r-hidden", teamOne, nil, `404: role "r-hidden" not found`},
		{"a team that does not exist", as("u-1"), nil, "r-read", meta.Owner{Kind: meta.OwnerTeam, ID: "t-9"}, nil, `400: team "t-9" does not exist`},
		{"a project that does not exist", as("u-1"), nil, "r-read", meta.Owner{Kind: meta.OwnerProject, ID: "p-9"}, nil, `400: project "p-9" does not exist`},
		{"a project the binder cannot see", as("u-1"), hiding("p-1"), "r-read", projectOne, nil, `404: project "p-1" not found`},
		{"the system scope", as("u-1"), nil, "r-read", system, []rolebinding.Subject{userSub("u-2")}, ""},
		{"a user that does not exist", as("u-1"), nil, "r-read", system, []rolebinding.Subject{userSub("u-1"), userSub("u-gone")}, `400: user "u-gone" does not exist`},
		{"a service account that does not exist", as("u-1"), nil, "r-read", system, []rolebinding.Subject{saSub("sa-gone")}, `400: service account "sa-gone" does not exist`},
		{"the user lookup failing", as("u-1"), nil, "r-read", system, []rolebinding.Subject{userSub("u-broken")}, "500: store down"},
	} {
		c := rows.checker(tc.authz)
		c.Rows.Role = func(_ context.Context, id string) *role.Role { return roles[id] }
		c.Rows.ServiceAccount, c.Rows.MissingUsers = keyRows(nil).Rows.ServiceAccount, keyRows(nil).Rows.MissingUsers
		rb := &rolebinding.RoleBinding{Meta: meta.Metadata{Owner: alice}, Spec: rolebinding.Spec{RoleID: tc.roleID, Scope: tc.scope, Subjects: tc.subjects}}
		wantRefused(t, tc.name, c.RoleBinding(tc.ctx, rb), tc.want)
		if rb.Meta.Owner != tc.scope {
			t.Errorf("%s: owner %+v, want the scope %+v", tc.name, rb.Meta.Owner, tc.scope)
		}
	}
}

// Editing a role that is already bound changes what every binding grants, so the editor must hold the new rules at each bound scope, as creating those bindings would have required.
func TestRoleEditRechecksEveryBinding(t *testing.T) {
	var asked []string
	c := Checker{
		Authz: teamGrant{teamID: "t-1", actions: map[string]bool{"keys.get": true}},
		Rows: Lookup{RoleBindingsFor: func(_ context.Context, roleID string) ([]*rolebinding.RoleBinding, error) {
			asked = append(asked, roleID)
			if roleID == "r-broken" {
				return nil, errors.New("store down")
			}
			return []*rolebinding.RoleBinding{{Spec: rolebinding.Spec{RoleID: roleID, Scope: teamOne}}}, nil
		}},
	}
	edit := func(id string, kinds, verbs []string) *role.Role {
		return &role.Role{Meta: meta.Metadata{ID: id, Name: "mine"}, Spec: role.Spec{Rules: []role.Rule{{Kinds: kinds, Verbs: verbs}}}}
	}
	ctx := as("u-1")
	wantRefused(t, "within what the editor holds", c.Role(ctx, edit("r-1", []string{"keys"}, []string{"get"})), "")
	err := c.Role(ctx, edit("r-1", []string{"keys"}, []string{"get", "delete"}))
	wantRefused(t, "widening a bound role", err, `403: authz: forbidden: binding role "mine" would grant keys.delete, which you do not hold at this scope`)
	if !errors.Is(err, authz.ErrForbidden) {
		t.Errorf("widening a bound role: err = %v, want it to wrap authz.ErrForbidden", err)
	}
	wantRefused(t, "the binding lookup failing", c.Role(ctx, edit("r-broken", nil, nil)), "500: store down")
	wantRefused(t, "no binding lookup", Checker{Authz: c.Authz}.Role(ctx, edit("r-1", []string{"*"}, []string{"*"})), "")
	if diff := cmp.Diff([]string{"r-1", "r-1", "r-broken"}, asked); diff != "" {
		t.Errorf("roles whose bindings were read (-want +got):\n%s", diff)
	}
}
