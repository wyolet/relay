package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/team"
)

// reachAuthz allows every authenticated caller except creating a policy in noCreate, and hides rows owned by hidden.
type reachAuthz struct {
	authz.AlwaysAllowAuthenticated
	noCreate, hidden meta.Owner
}

func (a reachAuthz) Authorize(ctx context.Context, action string, r authz.Resource) error {
	if action == "policies.create" && r.Owner != nil && *r.Owner == a.noCreate {
		return errors.New("forbidden")
	}
	return a.AlwaysAllowAuthenticated.Authorize(ctx, action, r)
}

func (a reachAuthz) Visible(_ context.Context, _, _ string, owner meta.Owner) bool {
	return owner != a.hidden
}

type reachFixture struct {
	h            http.Handler
	team, other  *team.Team
	pa, pb, hide *pool.Pool
	saved        *policy.Policy
	teamAcctID   string
}

// newReachFixture is host-a serving m1 and m2 and host-b serving m1, a pool per host, a pool of another team at host-b the caller cannot see, and a saved team policy linking pools a and b.
func newReachFixture(t *testing.T) *reachFixture {
	t.Helper()
	w := &plantest.World{}
	prov := w.Provider("acme")
	ha, hb := w.Host("host-a", "http://a.example"), w.Host("host-b", "http://b.example")
	m1, m2 := w.Model(prov, "m1"), w.Model(prov, "m2")
	w.Bind(m1, ha, adapters.OpenAI)
	w.Bind(m1, hb, adapters.OpenAI)
	w.Bind(m2, ha, adapters.OpenAI)
	f := &reachFixture{team: w.Team(), other: w.Team()}
	f.pa = w.Pool(ha, w.Credential(ha, w.Account(ha), plantest.System, "sk-a"))
	f.pb = w.Pool(hb, w.Credential(hb, w.Account(hb), plantest.System, "sk-b"))
	otherOwner := meta.Owner{Kind: meta.OwnerTeam, ID: f.other.Meta.ID}
	f.hide = w.Pool(hb, w.Credential(hb, w.Account(hb), plantest.System, "sk-hidden"))
	f.hide.Meta.Owner = otherOwner
	teamOwner := meta.Owner{Kind: meta.OwnerTeam, ID: f.team.Meta.ID}
	f.saved = w.Policy("team-default", policy.Spec{PoolIDs: []string{f.pa.Meta.ID, f.pb.Meta.ID}})
	f.saved.Meta.Owner = teamOwner
	w.BindPolicy(f.saved, w.TeamAccount(f.team))
	f.teamAcctID = w.TeamAccount(f.team).Meta.ID

	r := chi.NewRouter()
	r.Use(withTestActor("X-Test-Actor", map[string]*actor.Actor{"u": {UserID: meta.NewID(), Username: "u"}}))
	api := humachi.New(r, huma.DefaultConfig("reach-test", "0"))
	registerReachable(api, Deps{Authz: reachAuthz{noCreate: otherOwner, hidden: otherOwner}, PlanView: w.Load(t).Engine.View}, nil)
	f.h = r
	return f
}

func (f *reachFixture) get(t *testing.T, path string) (int, Reachable) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Test-Actor", "u")
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	var out Reachable
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return rec.Code, out
}

// A draft and the saved policy with the same owner and pools answer the same models, hosts and exclusions.
func TestReachable_DraftMatchesSaved(t *testing.T) {
	f := newReachFixture(t)
	code, draft := f.get(t, "/catalog/reachable?owner=team:"+f.team.Meta.ID+"&pool="+f.pa.Meta.ID+"&pool="+f.pb.Meta.ID)
	if code != http.StatusOK {
		t.Fatalf("draft = %d", code)
	}
	code, saved := f.get(t, "/policies/"+f.saved.Meta.ID+"/reachable")
	if code != http.StatusOK {
		t.Fatalf("saved = %d", code)
	}
	a, _ := json.Marshal([]any{draft.Models, draft.Excluded})
	b, _ := json.Marshal([]any{saved.Models, saved.Excluded})
	if string(a) != string(b) {
		t.Errorf("draft %s\nsaved %s", a, b)
	}
	if len(draft.Models) != 2 || draft.Account == nil || draft.Account.ID != f.teamAcctID {
		t.Errorf("draft = %+v, want m1 and m2 in the team's account", draft)
	}
	if len(saved.BoundAccounts) != 1 || saved.BoundAccounts[0].ID != f.teamAcctID {
		t.Errorf("bound = %+v, want the team account", saved.BoundAccounts)
	}
	if code, acct := f.get(t, "/accounts/"+f.teamAcctID+"/reachable"); code != http.StatusOK || len(acct.Models) != 2 {
		t.Errorf("account read = %d %+v", code, acct)
	}
}

// A draft owned where the caller may not create a policy is refused.
func TestReachable_OwnerNeedsCreate(t *testing.T) {
	f := newReachFixture(t)
	if code, _ := f.get(t, "/catalog/reachable?owner=team:"+f.other.Meta.ID); code != http.StatusForbidden {
		t.Errorf("owner the caller cannot create in = %d, want 403", code)
	}
	if code, _ := f.get(t, "/catalog/reachable?owner=host:x"); code != http.StatusUnprocessableEntity {
		t.Errorf("an owner kind policies cannot have = %d, want 422", code)
	}
}

// A pool the caller cannot read is left out of the draft, and so is everything only it brings.
func TestReachable_OmitsUnreadablePools(t *testing.T) {
	f := newReachFixture(t)
	_, r := f.get(t, "/catalog/reachable?owner=team:"+f.team.Meta.ID+"&pool="+f.pa.Meta.ID+"&pool="+f.hide.Meta.ID)
	for _, m := range r.Models {
		for _, h := range m.Hosts {
			if h.Name == "host-b" {
				t.Errorf("%s served at host-b, which only the hidden pool reaches", m.Model.Name)
			}
		}
		if slices.ContainsFunc(m.Via, func(v ReachVia) bool { return v.Pool != nil && v.Pool.ID == f.hide.Meta.ID }) {
			t.Errorf("%s names the hidden pool", m.Model.Name)
		}
	}
	if len(r.Models) != 2 {
		t.Errorf("models = %+v, want m1 and m2 through pool a", r.Models)
	}
}
