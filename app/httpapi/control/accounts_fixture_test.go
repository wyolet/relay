package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
)

// accountsWorld is a team with a project, two members of the project with a key each, and a priced model served through a system credential. The team's account caps requests shared, the project's account gives each lane a budget and a model-scoped cap, and the first member's account has a small cap of its own.
type accountsWorld struct {
	w            *plantest.World
	team         *team.Team
	proj         *project.Project
	u1, u2       *user.User
	m1, m2       *account.Account
	k1, k2       *key.Key
	teamRL       *ratelimit.RateLimit
	laneRL       *ratelimit.RateLimit
	teamPol      *policy.Policy
	lanePol      *policy.Policy
	projBudgetID string
}

func fixedRule(amount int64) ratelimit.Rule {
	return ratelimit.Rule{Meter: ratelimit.MeterRequests, Amount: amount, Window: ratelimit.Window(time.Hour), Strategy: ratelimit.StrategyFixedWindow}
}

func newAccountsWorld() *accountsWorld {
	w := &plantest.World{}
	prov := w.Provider("prov")
	h := w.Host("h", "http://h.invalid")
	m := w.Model(prov, "m1")
	w.Bind(m, h, adapters.OpenAI)
	w.Price(m, h, 1)
	w.Credential(h, nil, plantest.System, "sk-system")

	aw := &accountsWorld{w: w}
	aw.team = w.Team()
	aw.proj = w.Project(aw.team)
	scope := meta.Owner{Kind: meta.OwnerProject, ID: aw.proj.Meta.ID}
	aw.u1, aw.u2 = w.User(), w.User()
	aw.m1, aw.m2 = w.Member(aw.u1, scope), w.Member(aw.u2, scope)
	aw.k1, aw.k2 = w.KeyIn(aw.u1, aw.m1, "sk-one"), w.KeyIn(aw.u2, aw.m2, "sk-two")
	aw.k1.Spec.Prefix = "sk-wr-one"

	no := false
	aw.teamRL = w.RateLimit(fixedRule(100))
	aw.teamPol = w.Policy("team", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: aw.teamRL.Meta.ID}}})
	w.BindPolicy(aw.teamPol, w.TeamAccount(aw.team))

	projRL := w.RateLimit(fixedRule(50))
	b := w.Budget("10")
	aw.projBudgetID = b.Meta.ID
	projPol := w.Policy("project", policy.Spec{
		RateLimits: []policy.RateLimitLink{{RateLimitID: projRL.Meta.ID, Models: []string{"prov/m1"}, Shared: &no}},
		Budgets:    []policy.BudgetLink{{BudgetID: b.Meta.ID, Shared: &no}},
	})
	w.BindPolicy(projPol, w.ProjectAccount(aw.proj))

	aw.laneRL = w.RateLimit(fixedRule(4))
	aw.lanePol = w.Policy("lane", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: aw.laneRL.Meta.ID}}})
	w.BindPolicy(aw.lanePol, aw.m1)
	return aw
}

// accountsHarness serves the account reads over a loaded engine.
type accountsHarness struct {
	h     http.Handler
	plans *plantest.Plans
}

func newAccountsHarness(t *testing.T, w *plantest.World, az authz.Authorizer, actors map[string]*actor.Actor) *accountsHarness {
	t.Helper()
	plans := w.Load(t)
	r := chi.NewRouter()
	r.Use(withTestActor("X-Test-Actor", actors))
	api := humachi.New(r, huma.DefaultConfig("accounts-test", "0"))
	d := Deps{Authz: az, PlanView: plans.Engine.View}
	registerMyAccounts(api, d, nil)
	return &accountsHarness{h: r, plans: plans}
}

func (ah *accountsHarness) get(t *testing.T, who, path string, out any) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Test-Actor", who)
	w := httptest.NewRecorder()
	ah.h.ServeHTTP(w, req)
	if out != nil && w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("decode %s: %v", w.Body.String(), err)
		}
	}
	return w.Code
}
