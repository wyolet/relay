package control

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/meta"
)

// The personal account first, then member accounts and provider accounts, each by name; another user's accounts, the team's and project's own, the accounts of the user's own service accounts, and revoked keys are left out.
func TestMyAccounts(t *testing.T) {
	aw := newAccountsWorld()
	// A closed member account opened on first use, which no role binding follows, and a provider account u1 owns.
	tokenOnly := account.Member(aw.u1.ID, aw.u1.Username, meta.Owner{Kind: meta.OwnerTeam, ID: aw.team.Meta.ID}, aw.team.Meta.Name, "")
	tokenOnly.Meta.ID, tokenOnly.Meta.Name, tokenOnly.Spec.Closed = meta.NewID(), aw.m1.Meta.Name+"-z", true
	prov := &account.Account{Meta: meta.Metadata{ID: meta.NewID(), Name: "u1-provider", Owner: meta.Owner{Kind: meta.OwnerUser, ID: aw.u1.ID}}, Spec: account.Spec{HostID: aw.w.Hosts[0].Meta.ID}}
	// u1's service account, its account and a share it holds: all owned by u1, none of them u1's own.
	bot := aw.w.UserServiceAccount(aw.u1)
	botShare := account.Share(prov, account.Receiver{Kind: account.ReceiverServiceAccount, ID: bot.Meta.ID}, meta.Owner{Kind: meta.OwnerUser, ID: aw.u1.ID}, bot.Meta.Name)
	botShare.Meta.ID = meta.NewID()
	aw.w.Accounts = append(aw.w.Accounts, tokenOnly, prov, botShare)
	revoked := aw.w.KeyIn(aw.u1, aw.m1, "sk-revoked")
	revoked.Spec.RevokedAt = &time.Time{}
	personal := aw.w.PersonalAccount(aw.u1)
	personalKey := aw.w.UserKey(aw.u1, "sk-personal")
	actors := map[string]*actor.Actor{"u1": {UserID: aw.u1.ID}, "token": {AdminToken: true}}
	ah := newAccountsHarness(t, aw.w, testRBAC(), actors)

	row := func(a *account.Account, kind account.Kind, scope AccountScope, keys ...AccountKey) MyAccount {
		return MyAccount{Account: AccountSummary{ID: a.Meta.ID, Name: a.Meta.Name, DisplayName: a.Meta.DisplayName, Closed: a.Spec.Closed}, Kind: kind, Scope: scope, Keys: append([]AccountKey{}, keys...)}
	}
	self := AccountScope{Kind: meta.OwnerUser, ID: aw.u1.ID, Name: aw.u1.Username}
	member := row(aw.m1, account.KindMember, AccountScope{Kind: meta.OwnerProject, ID: aw.proj.Meta.ID, Name: aw.proj.Meta.Name}, AccountKey{ID: aw.k1.Meta.ID, Name: aw.k1.Meta.Name, Prefix: "sk-wr-one"})
	member.Policy = &BoundPolicy{ID: aw.lanePol.Meta.ID, Name: aw.lanePol.Meta.Name}
	opened := row(tokenOnly, account.KindMember, AccountScope{Kind: meta.OwnerTeam, ID: aw.team.Meta.ID, Name: aw.team.Meta.Name})
	opened.TokenOnly = true
	want := []MyAccount{
		row(personal, account.KindPersonal, self, AccountKey{ID: personalKey.Meta.ID, Name: personalKey.Meta.Name}),
		member, opened,
		row(prov, account.KindProvider, self),
	}
	var got []MyAccount
	if code := ah.get(t, "u1", "/me/accounts", &got); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("accounts (-want +got):\n%s", diff)
	}

	var none []MyAccount
	if code := ah.get(t, "token", "/me/accounts", &none); code != http.StatusOK || none == nil || len(none) != 0 {
		t.Errorf("admin token: %d %v, want an empty list", code, none)
	}
	if code := ah.get(t, "", "/me/accounts", nil); code != http.StatusUnauthorized {
		t.Errorf("anonymous: %d, want 401", code)
	}
}

func TestMyAccountsBeforePlansLoad(t *testing.T) {
	for _, view := range []func() *engine.View{nil, func() *engine.View { return nil }} {
		r, api := testAPI(t, scopeActors)
		registerMyAccounts(api, Deps{Authz: testRBAC(), PlanView: view}, nil)
		wantAPIError(t, scopeReq(t, r, "alice", http.MethodGet, "/me/accounts", ""), http.StatusServiceUnavailable, "", "plans are not loaded yet")
	}
}
