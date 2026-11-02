package control

import (
	"context"
	"net/http"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/user"
)

const testUSD = int64(1_000_000_000)

// spend runs n requests of k for m1 through the route's first slot, each settled at cost.
func (ah *accountsHarness) spend(t *testing.T, k *key.Key, n int, cost int64) {
	t.Helper()
	route := ah.plans.Caller(t, k).Routes.Get("m1")
	if route.Refused != nil || len(route.Hops) == 0 {
		t.Fatalf("route refused: %v", route.Refused)
	}
	for range n {
		res, err := ah.lim.Admit(context.Background(), route.Hops[0].Counters[0], limits.Usage{})
		if err != nil {
			t.Fatalf("admit: %v", err)
		}
		res.Settle(limits.Usage{CostNanos: cost})
	}
}

func TestAccountLimits_LadderSharedAndEach(t *testing.T) {
	aw := newAccountsWorld()
	actors := map[string]*actor.Actor{"u1": {UserID: aw.u1.ID}, "u2": {UserID: aw.u2.ID}}
	ah := newAccountsHarness(t, aw.w, authz.AlwaysAllowAuthenticated{}, actors)
	ah.spend(t, aw.k1, 3, testUSD)
	ah.spend(t, aw.k2, 2, 2*testUSD)

	var got AccountLimits
	if code := ah.get(t, "u1", "/accounts/"+aw.m1.Meta.ID+"/limits", &got); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if got.Lane.ID != aw.m1.Meta.ID || got.Lane.Kind != account.KindMember {
		t.Fatalf("lane = %+v", got.Lane)
	}
	if len(got.Rungs) != 3 {
		t.Fatalf("rungs = %+v, want team, project and lane", got.Rungs)
	}
	teamRung, projRung, laneRung := got.Rungs[0], got.Rungs[1], got.Rungs[2]
	if teamRung.Account.Kind != account.KindTeam || projRung.Account.Kind != account.KindProject || laneRung.Account.ID != aw.m1.Meta.ID {
		t.Fatalf("rung accounts = %v, %v, %v", teamRung.Account, projRung.Account, laneRung.Account)
	}
	if teamRung.Policy == nil || teamRung.Policy.ID != aw.teamPol.Meta.ID {
		t.Errorf("team rung policy = %+v", teamRung.Policy)
	}

	// Shared on the team account: both members' requests count.
	if rl := teamRung.RateLimits[0]; !rl.Shared || rl.Rules[0].Used != 5 || rl.Rules[0].Remaining != 95 || rl.Models == nil || len(rl.Models) != 0 {
		t.Errorf("team rate limit = %+v", rl)
	}
	// Each on the project account: only this lane's.
	if rl := projRung.RateLimits[0]; rl.Shared || rl.Rules[0].Used != 3 || rl.Rules[0].Window != 3600 || len(rl.Models) != 1 || rl.Models[0] != "prov/m1" {
		t.Errorf("project rate limit = %+v", rl)
	}
	if b := projRung.Budgets[0]; b.Shared || b.SpentUSD != "3" || b.RemainingUSD != "7" || b.AmountUSD != "10" || b.Period != "month" || b.Mode != "hard" {
		t.Errorf("project budget = %+v", b)
	}
	if rl := laneRung.RateLimits[0]; rl.Rules[0].Used != 3 || rl.Rules[0].Remaining != 1 {
		t.Errorf("lane rate limit = %+v", rl)
	}
	// A quarter of the lane's cap left beats 70% of the budget and 95% of the team's cap.
	if tt := got.Tightest; tt == nil || tt.Kind != "rateLimit" || tt.ID != aw.laneRL.Meta.ID || tt.AccountID != aw.m1.Meta.ID || tt.Remaining != 1 || tt.Label != "1 requests left: member "+aw.laneRL.Meta.Name {
		t.Errorf("tightest = %+v", got.Tightest)
	}

	// The second member's account has no policy of its own: the project budget is tightest, and its spend is this lane's alone.
	var other AccountLimits
	if code := ah.get(t, "u2", "/keys/"+aw.k2.Meta.ID+"/limits", &other); code != http.StatusOK {
		t.Fatalf("key limits status %d", code)
	}
	if other.Lane.ID != aw.m2.Meta.ID || len(other.Rungs) != 3 || other.Rungs[2].Policy != nil {
		t.Fatalf("key limits = %+v", other)
	}
	if b := other.Rungs[1].Budgets[0]; b.SpentUSD != "4" || b.RemainingUSD != "6" {
		t.Errorf("second lane's budget = %+v, want its own 4 spent", b)
	}
	if tt := other.Tightest; tt == nil || tt.Kind != "budget" || tt.ID != aw.projBudgetID || tt.Remaining != 6 || tt.Label != "$6 left: project "+other.Rungs[1].Budgets[0].Name {
		t.Errorf("tightest = %+v", other.Tightest)
	}
}

func TestAccountLimits_Visibility(t *testing.T) {
	aw := newAccountsWorld()
	actors := map[string]*actor.Actor{
		"u1":    {UserID: aw.u1.ID},
		"u2":    {UserID: aw.u2.ID},
		"root":  {UserID: "u-root", Roles: []string{user.RoleAdmin}},
		"token": {AdminToken: true},
	}
	ah := newAccountsHarness(t, aw.w, testRBAC(), actors)
	m1 := "/accounts/" + aw.m1.Meta.ID + "/limits"
	k1 := "/keys/" + aw.k1.Meta.ID + "/limits"
	teamAcc := "/accounts/" + aw.w.TeamAccount(aw.team).Meta.ID + "/limits"
	for _, tc := range []struct {
		who, path string
		want      int
	}{
		{"u1", m1, http.StatusOK}, // acts in it, though the project owns it
		{"u2", m1, http.StatusNotFound},
		{"u1", "/accounts/" + aw.m1.Meta.Name + "/limits", http.StatusOK},
		{"u1", k1, http.StatusOK},
		{"u1", "/keys/" + aw.k1.Meta.Name + "/limits", http.StatusOK},
		{"u2", k1, http.StatusNotFound},
		{"token", k1, http.StatusOK},
		{"root", m1, http.StatusOK},
		{"u1", teamAcc, http.StatusNotFound},
		{"root", teamAcc, http.StatusBadRequest},
		{"u1", "/accounts/nope/limits", http.StatusNotFound},
	} {
		if got := ah.get(t, tc.who, tc.path, nil); got != tc.want {
			t.Errorf("%s GET %s = %d, want %d", tc.who, tc.path, got, tc.want)
		}
	}
}

func TestUSDStrings(t *testing.T) {
	for _, tc := range []struct {
		nanos       int64
		exact, cent string
	}{
		{0, "0", "0"},
		{50 * testUSD, "50", "50"},
		{49*testUSD + testUSD/2, "49.5", "49.50"},
		{1_234_567_891, "1.234567891", "1.23"},
		{5, "0.000000005", "0"},
	} {
		if got := usdString(tc.nanos); got != tc.exact {
			t.Errorf("usdString(%d) = %s, want %s", tc.nanos, got, tc.exact)
		}
		if got := usdLabel(tc.nanos); got != tc.cent {
			t.Errorf("usdLabel(%d) = %s, want %s", tc.nanos, got, tc.cent)
		}
	}
}
