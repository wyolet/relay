package control

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
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

// wantLimits builds the answers the limits reads give over newAccountsWorld.
type wantLimits struct{ aw *accountsWorld }

func limitsOf(a *account.Account, kind account.Kind) LimitsAccount {
	return LimitsAccount{ID: a.Meta.ID, Name: a.Meta.Name, Kind: kind}
}

// rateLimit is rl's link with used counted against its one rule; used -1 is a per-lane link read on a scope account, which has no count.
func (wantLimits) rateLimit(rl *ratelimit.RateLimit, shared bool, used int64, models ...string) []LimitsRateLimit {
	rule := LimitsRule{Meter: ratelimit.MeterRequests, Strategy: ratelimit.StrategyFixedWindow, Amount: rl.Spec.Rules[0].Amount, Window: 3600}
	if used >= 0 {
		left := rule.Amount - used
		rule.Used, rule.Remaining = &used, &left
	}
	return []LimitsRateLimit{{RateLimitID: rl.Meta.ID, Name: rl.Meta.Name, Models: append([]string{}, models...), Shared: shared, PerLane: used < 0, Rules: []LimitsRule{rule}}}
}

func (w wantLimits) teamRung(used int64) LimitsRung {
	return LimitsRung{
		Account: limitsOf(w.aw.w.TeamAccount(w.aw.team), account.KindTeam), Policy: &BoundPolicy{ID: w.aw.teamPol.Meta.ID, Name: w.aw.teamPol.Meta.Name},
		RateLimits: w.rateLimit(w.aw.teamRL, true, used), Budgets: []LimitsBudget{},
	}
}

// projectRung is the project's rung as one lane reads it; used -1 reads it on the project's own account.
func (w wantLimits) projectRung(used int64, spent, left string) LimitsRung {
	b := w.aw.projBudget
	return LimitsRung{
		Account: limitsOf(w.aw.w.ProjectAccount(w.aw.proj), account.KindProject), Policy: &BoundPolicy{ID: w.aw.projPol.Meta.ID, Name: w.aw.projPol.Meta.Name},
		RateLimits: w.rateLimit(w.aw.projRL, false, used, "prov/m1"),
		Budgets:    []LimitsBudget{{BudgetID: b.Meta.ID, Name: b.Meta.Name, PerLane: used < 0, AmountUSD: "10", Period: "month", Mode: "hard", SpentUSD: spent, RemainingUSD: left}},
	}
}

// Shared links read what every lane under the account spent together, the others this lane's own; tightest is the hard limit with the least left as a share of its cap. A team's or project's own account reads the accounts above and including it, its per-lane links marked and left out of tightest.
func TestAccountLimits(t *testing.T) {
	aw := newAccountsWorld()
	actors := map[string]*actor.Actor{"u1": {UserID: aw.u1.ID}, "u2": {UserID: aw.u2.ID}, "root": {UserID: "u-root"}}
	ah := newAccountsHarness(t, aw.w, authz.AlwaysAllowAuthenticated{}, actors)
	ah.spend(t, aw.k1, 3, testUSD)
	ah.spend(t, aw.k2, 2, 2*testUSD)
	w := wantLimits{aw}
	teamAcc, projAcc := aw.w.TeamAccount(aw.team), aw.w.ProjectAccount(aw.proj)
	teamCap := &LimitsTightest{AccountID: teamAcc.Meta.ID, Kind: "rateLimit", ID: aw.teamRL.Meta.ID, Remaining: 95, Label: "95 requests left: team " + aw.teamRL.Meta.Name}

	for _, tc := range []struct {
		name, who, path string
		want            AccountLimits
	}{
		{"a lane with a cap of its own", "u1", "/accounts/" + aw.m1.Meta.ID + "/limits", AccountLimits{
			Lane: limitsOf(aw.m1, account.KindMember),
			Rungs: []LimitsRung{w.teamRung(5), w.projectRung(3, "3", "7"), {
				Account: limitsOf(aw.m1, account.KindMember), Policy: &BoundPolicy{ID: aw.lanePol.Meta.ID, Name: aw.lanePol.Meta.Name},
				RateLimits: w.rateLimit(aw.laneRL, true, 3), Budgets: []LimitsBudget{},
			}},
			// A quarter of the lane's cap left beats 70% of the budget and 95% of the team's cap.
			Tightest: &LimitsTightest{AccountID: aw.m1.Meta.ID, Kind: "rateLimit", ID: aw.laneRL.Meta.ID, Remaining: 1, Label: "1 requests left: member " + aw.laneRL.Meta.Name},
		}},
		{"a key's default lane, with no policy of its own", "u2", "/keys/" + aw.k2.Meta.ID + "/limits", AccountLimits{
			Lane:     limitsOf(aw.m2, account.KindMember),
			Rungs:    []LimitsRung{w.teamRung(5), w.projectRung(2, "4", "6"), {Account: limitsOf(aw.m2, account.KindMember), RateLimits: []LimitsRateLimit{}, Budgets: []LimitsBudget{}}},
			Tightest: &LimitsTightest{AccountID: projAcc.Meta.ID, Kind: "budget", ID: aw.projBudget.Meta.ID, Remaining: 6, Label: "$6 left: project " + aw.projBudget.Meta.Name},
		}},
		{"the project's own account", "root", "/accounts/" + projAcc.Meta.ID + "/limits", AccountLimits{
			Lane: limitsOf(projAcc, account.KindProject), Rungs: []LimitsRung{w.teamRung(5), w.projectRung(-1, "", "")}, Tightest: teamCap,
		}},
		{"the team's own account, by slug", "root", "/accounts/" + teamAcc.Meta.Name + "/limits", AccountLimits{
			Lane: limitsOf(teamAcc, account.KindTeam), Rungs: []LimitsRung{w.teamRung(5)}, Tightest: teamCap,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got AccountLimits
			if code := ah.get(t, tc.who, tc.path, &got); code != http.StatusOK {
				t.Fatalf("status %d", code)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("limits (-want +got):\n%s", diff)
			}
		})
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
		{"root", teamAcc, http.StatusOK},
		{"u1", "/accounts/nope/limits", http.StatusNotFound},
	} {
		if got := ah.get(t, tc.who, tc.path, nil); got != tc.want {
			t.Errorf("%s GET %s = %d, want %d", tc.who, tc.path, got, tc.want)
		}
	}
}

// Counts come from the data plane's limiter: without one the read is refused, not answered with zeros.
func TestAccountLimitsWithoutALimiter(t *testing.T) {
	aw := newAccountsWorld()
	r, api := testAPI(t, map[string]*actor.Actor{"u1": {UserID: aw.u1.ID}})
	registerLimits(api, Deps{Authz: authz.AlwaysAllowAuthenticated{}, PlanView: aw.w.Load(t).Engine.View}, nil)
	wantAPIError(t, scopeReq(t, r, "u1", http.MethodGet, "/accounts/"+aw.m1.Meta.ID+"/limits", ""), http.StatusServiceUnavailable, "", "no limiter to read counts from")
}

func TestMeterNoun(t *testing.T) {
	for m, want := range map[ratelimit.Meter]string{
		ratelimit.MeterRequests:        "requests",
		ratelimit.MeterConcurrency:     "concurrent requests",
		ratelimit.MeterTokens:          "tokens",
		ratelimit.MeterTokensInput:     "input tokens",
		ratelimit.MeterTokensCacheRead: "cache read tokens",
	} {
		if got := meterNoun(m); got != want {
			t.Errorf("meterNoun(%s) = %q, want %q", m, got, want)
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
