package control

import (
	"net/http"
	"testing"
	"time"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/meta"
)

func TestMyAccounts(t *testing.T) {
	aw := newAccountsWorld()
	// A member account opened on first use, which no role binding follows, and a provider account u1 owns.
	tokenOnly := account.Member(aw.u1.ID, aw.u1.Username, meta.Owner{Kind: meta.OwnerTeam, ID: aw.team.Meta.ID}, aw.team.Meta.Name, "")
	tokenOnly.Meta.ID = meta.NewID()
	prov := &account.Account{Meta: meta.Metadata{ID: meta.NewID(), Name: "u1-provider", Owner: meta.Owner{Kind: meta.OwnerUser, ID: aw.u1.ID}}, Spec: account.Spec{HostID: aw.w.Hosts[0].Meta.ID}}
	aw.w.Accounts = append(aw.w.Accounts, tokenOnly, prov)
	revoked := aw.w.KeyIn(aw.u1, aw.m1, "sk-revoked")
	revoked.Spec.RevokedAt = &time.Time{}
	actors := map[string]*actor.Actor{"u1": {UserID: aw.u1.ID}, "token": {AdminToken: true}}
	ah := newAccountsHarness(t, aw.w, testRBAC(), actors)

	var got []MyAccount
	if code := ah.get(t, "u1", "/me/accounts", &got); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if len(got) != 4 || got[0].Kind != account.KindPersonal || got[1].Kind != account.KindMember || got[2].Kind != account.KindMember || got[3].Kind != account.KindProvider {
		t.Fatalf("accounts = %+v, want personal, two member, provider", got)
	}
	byID := map[string]MyAccount{}
	for _, a := range got {
		byID[a.Account.ID] = a
	}
	m := byID[aw.m1.Meta.ID]
	if m.TokenOnly || m.Account.Closed || m.Scope.Kind != meta.OwnerProject || m.Scope.ID != aw.proj.Meta.ID || m.Scope.Name != aw.proj.Meta.Name {
		t.Errorf("member account = %+v", m)
	}
	if m.Policy == nil || m.Policy.ID != aw.lanePol.Meta.ID || len(m.Keys) != 1 || m.Keys[0].ID != aw.k1.Meta.ID || m.Keys[0].Prefix != "sk-wr-one" {
		t.Errorf("member account policy %+v, keys %+v; want the lane policy and k1 alone", m.Policy, m.Keys)
	}
	if to := byID[tokenOnly.Meta.ID]; !to.TokenOnly || to.Scope.Kind != meta.OwnerTeam || to.Policy != nil || to.Keys == nil || len(to.Keys) != 0 {
		t.Errorf("token-only account = %+v", to)
	}
	if p := byID[prov.Meta.ID]; p.Scope.Kind != meta.OwnerUser || p.Scope.Name != aw.u1.Username {
		t.Errorf("provider account = %+v", p)
	}
	if _, ok := byID[aw.m2.Meta.ID]; ok {
		t.Error("another user's member account is listed")
	}

	var none []MyAccount
	if code := ah.get(t, "token", "/me/accounts", &none); code != http.StatusOK || none == nil || len(none) != 0 {
		t.Errorf("admin token: %d %v, want an empty list", code, none)
	}
}
