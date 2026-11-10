package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/meta"
)

// reachAuthz allows every authenticated caller except writing a policy in noCreate, and hides rows owned by hidden.
type reachAuthz struct {
	authz.AlwaysAllowAuthenticated
	noCreate, hidden meta.Owner
}

func (a reachAuthz) Authorize(ctx context.Context, action string, r authz.Resource) error {
	if strings.HasPrefix(action, "policies.") && r.Owner != nil && *r.Owner == a.noCreate {
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
	for i, m := range saved.Models {
		if len(m.Accounts) != 1 || m.Accounts[0].ID != f.teamAcctID {
			t.Errorf("saved %s reached by %+v, want the team account", m.Model.Name, m.Accounts)
		}
		saved.Models[i].Accounts = nil
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
	if len(r.Dropped) != 1 || r.Dropped[0].Code != "not_found" || r.Dropped[0].Pool.ID != f.hide.Meta.ID || r.Dropped[0].Pool.Name != "" {
		t.Errorf("dropped = %+v, want the hidden pool by id alone", r.Dropped)
	}
}

// saShareFixture is project policy web-ci bound to service account ci-bot, whose pool lists an account shared to ci-bot.
type saShareFixture struct {
	reachFixture
	proj   string
	pool   *pool.Pool
	pol    *policy.Policy
	saAcct string
}

func newSAShareFixture(t *testing.T) *saShareFixture {
	t.Helper()
	w := &plantest.World{}
	h := w.Host("openai", "http://openai.example")
	w.Bind(w.Model(w.Provider("acme"), "gpt-4o-mini"), h, adapters.OpenAI)
	real := w.Account(h)
	w.Credential(h, real, plantest.System, "sk-real")
	tm := w.Team()
	p := w.Project(tm)
	sa := w.ServiceAccount(p)
	projOwner := meta.Owner{Kind: meta.OwnerProject, ID: p.Meta.ID}
	share := account.Share(real, account.Receiver{Kind: account.ReceiverServiceAccount, ID: sa.Meta.ID}, projOwner, sa.Meta.Name)
	share.Meta.ID = meta.NewID()
	w.Accounts = append(w.Accounts, share)
	f := &saShareFixture{proj: p.Meta.ID, saAcct: w.ServiceAccountAccount(sa).Meta.ID}
	f.pool = w.Pool(h)
	f.pool.Meta.Owner = projOwner
	f.pool.Spec.AccountIDs = []string{share.Meta.ID}
	f.pol = w.Policy("web-ci", policy.Spec{PoolIDs: []string{f.pool.Meta.ID}})
	f.pol.Meta.Owner = projOwner
	w.BindPolicy(f.pol, w.ServiceAccountAccount(sa))
	f.team = w.Team()
	f.teamAcctID = w.TeamAccount(f.team).Meta.ID

	r := chi.NewRouter()
	r.Use(withTestActor("X-Test-Actor", map[string]*actor.Actor{"u": {UserID: meta.NewID(), Username: "u"}}))
	api := humachi.New(r, huma.DefaultConfig("reach-test", "0"))
	registerReachable(api, Deps{Authz: authz.AlwaysAllowAuthenticated{}, PlanView: w.Load(t).Engine.View}, nil)
	f.h = r
	return f
}

// Read in the project's account, the share serves nothing, and dropped says why and what to pass.
func TestReachable_SAShare_DraftInProjectAccount(t *testing.T) {
	f := newSAShareFixture(t)
	_, r := f.get(t, "/catalog/reachable?owner=project:"+f.proj+"&pool="+f.pool.Meta.ID)
	if len(r.Models) != 0 {
		t.Fatalf("models = %+v, want none in the project's account", r.Models)
	}
	if len(r.Dropped) != 1 || r.Dropped[0].Code != "held_for_another_lane" || !strings.HasSuffix(r.Dropped[0].Reason, "pass account="+f.saAcct) {
		t.Errorf("dropped = %+v, want the share held for ci-bot", r.Dropped)
	}
}

// With account set to the service account's, the draft reaches gpt-4o-mini; an account outside the owner scope is a 400.
func TestReachable_SAShare_DraftInItsAccount(t *testing.T) {
	f := newSAShareFixture(t)
	code, r := f.get(t, "/catalog/reachable?owner=project:"+f.proj+"&pool="+f.pool.Meta.ID+"&account="+f.saAcct)
	if code != http.StatusOK || len(r.Models) != 1 || r.Models[0].Model.Name != "gpt-4o-mini" || len(r.Dropped) != 0 {
		t.Errorf("%d models %+v dropped %+v, want gpt-4o-mini", code, r.Models, r.Dropped)
	}
	if code, _ := f.get(t, "/catalog/reachable?owner=project:"+f.proj+"&account="+f.teamAcctID); code != http.StatusBadRequest {
		t.Errorf("an account of another team = %d, want 400", code)
	}
}

// The saved policy is read in the service account it is bound to.
func TestReachable_SAShare_Saved(t *testing.T) {
	f := newSAShareFixture(t)
	code, r := f.get(t, "/policies/"+f.pol.Meta.ID+"/reachable")
	if code != http.StatusOK || len(r.Models) != 1 || len(r.Models[0].Accounts) != 1 || r.Models[0].Accounts[0].ID != f.saAcct {
		t.Errorf("%d %+v, want gpt-4o-mini reached by ci-bot's account", code, r)
	}
}
