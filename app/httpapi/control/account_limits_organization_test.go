package control

import (
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/meta"
)

// The limits read shows the numbers of every limit a lane's requests run under, but names the organization's policy, rate limit and budget only to a caller who may see them.
func TestAccountLimits_OrganizationRowsUnnamed(t *testing.T) {
	aw := newAccountsWorld()
	org := meta.Owner{Kind: meta.OwnerOrganization}
	aw.teamPol.Meta.Owner, aw.teamRL.Meta.Owner, aw.projBudget.Meta.Owner = org, org, org
	aw.teamRL.Spec.Rules[0].Amount = 6
	actors := map[string]*actor.Actor{"u1": {UserID: aw.u1.ID}, "root": {UserID: "u-root", Roles: []string{user.RoleAdmin}}}
	ah := newAccountsHarness(t, aw.w, testRBAC(), actors)
	ah.spend(t, aw.k1, 3, testUSD)
	ah.spend(t, aw.k2, 2, 2*testUSD)
	path := "/accounts/" + aw.m1.Meta.ID + "/limits"

	var asU1, asRoot AccountLimits
	if code := ah.get(t, "u1", path, &asU1); code != http.StatusOK {
		t.Fatalf("u1: status %d", code)
	}
	if code := ah.get(t, "root", path, &asRoot); code != http.StatusOK {
		t.Fatalf("root: status %d", code)
	}

	teamRung, projRung := asU1.Rungs[0], asU1.Rungs[1]
	if teamRung.Policy == nil || *teamRung.Policy != (BoundPolicy{Hidden: true}) {
		t.Errorf("team policy = %+v, want bound and hidden", teamRung.Policy)
	}
	rl := teamRung.RateLimits[0]
	if !rl.Hidden || rl.RateLimitID != "" || rl.Name != "" || len(rl.Rules) != 1 || rl.Rules[0].Amount != 6 || *rl.Rules[0].Used != 5 {
		t.Errorf("team rate limit = %+v, want its rule and count without id or name", rl)
	}
	b := projRung.Budgets[0]
	if !b.Hidden || b.BudgetID != "" || b.Name != "" || b.AmountUSD != "10" || b.SpentUSD != "3" {
		t.Errorf("project budget = %+v, want its amount and spend without id or name", b)
	}
	teamAcc := aw.w.TeamAccount(aw.team)
	want := &LimitsTightest{AccountID: teamAcc.Meta.ID, Kind: "rateLimit", Remaining: 1, Label: "1 requests left: " + string(account.KindTeam) + " rate limit"}
	if diff := cmp.Diff(want, asU1.Tightest); diff != "" {
		t.Errorf("tightest (-want +got):\n%s", diff)
	}

	// An admin reads the same numbers with every name.
	if p := asRoot.Rungs[0].Policy; p == nil || p.ID != aw.teamPol.Meta.ID || p.Hidden {
		t.Errorf("admin: team policy = %+v, want it named", p)
	}
	if rl := asRoot.Rungs[0].RateLimits[0]; rl.Hidden || rl.RateLimitID != aw.teamRL.Meta.ID {
		t.Errorf("admin: team rate limit = %+v, want it named", rl)
	}
	if asRoot.Tightest == nil || asRoot.Tightest.ID != aw.teamRL.Meta.ID {
		t.Errorf("admin: tightest = %+v, want the team rate limit by id", asRoot.Tightest)
	}
}
