package plan_test

import (
	"testing"
	"time"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/engine"
)

// org is one provider serving m1, m2 and m3 at one priced host, and a team with a project, a service account and its key.
type org struct {
	w      *plantest.World
	prov   *provider.Provider
	host   *host.Host
	models map[string]*model.Model
	team   *team.Team
	proj   *project.Project
	sa     *serviceaccount.ServiceAccount
	saKey  *key.Key
}

func newOrg() *org {
	w := &plantest.World{}
	o := &org{w: w, prov: w.Provider("prov"), models: map[string]*model.Model{}}
	o.host = w.Host("h", "http://h.invalid")
	for _, name := range []string{"m1", "m2", "m3"} {
		m := w.Model(o.prov, name)
		w.Bind(m, o.host, adapters.OpenAI)
		w.Price(m, o.host, 1)
		o.models[name] = m
	}
	o.team = w.Team()
	o.proj = w.Project(o.team)
	o.sa = w.ServiceAccount(o.proj)
	o.saKey = w.ServiceAccountKey(o.sa, "sk-sa-"+o.sa.Meta.ID)
	return o
}

func ownedBy(kind meta.OwnerKind, id string) meta.Owner { return meta.Owner{Kind: kind, ID: id} }

// teamAcct, projectAcct and saAcct are the accounts of the org's team, project and service account, which bindings bind.
func (o *org) teamAcct() *account.Account    { return o.w.TeamAccount(o.team) }
func (o *org) projectAcct() *account.Account { return o.w.ProjectAccount(o.proj) }
func (o *org) saAcct() *account.Account      { return o.w.ServiceAccountAccount(o.sa) }

// routeCredentials lists the credential names a route reaches, hop by hop, slot by slot.
func routeCredentials(r *plan.Route) []string {
	var out []string
	for i := range r.Hops {
		for _, c := range r.Hops[i].Credentials {
			out = append(out, c.Meta.Name)
		}
	}
	return out
}

// callerCredentials lists the credential names the caller's pools hold, pool by pool.
func callerCredentials(cp *plan.CallerPlan) []string {
	var out []string
	for _, p := range cp.Pools {
		for _, s := range p.Slots {
			out = append(out, s.Credential.Meta.Name)
		}
	}
	return out
}

func sameList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mustRoute(t *testing.T, cp *plan.CallerPlan, ref string) *plan.Route {
	t.Helper()
	r := cp.Routes.Get(ref)
	if r.Refused != nil {
		t.Fatalf("route %s refused: %v", ref, r.Refused)
	}
	if len(r.Hops) == 0 {
		t.Fatalf("route %s has no hops", ref)
	}
	return r
}

// wantRefusal checks that a route is refused with code by the policy on the named rung.
func wantRefusal(t *testing.T, r *plan.Route, code string, rung limits.Rung, policyID string) {
	t.Helper()
	if r.Refused == nil {
		t.Fatalf("route was served by %v, want a %s refusal on the %s rung", routeCredentials(r), code, rung)
	}
	if r.Refused.Code != code || r.Refused.Rung != rung || r.Refused.PolicyID != policyID {
		t.Fatalf("refusal = %+v, want code %s on the %s rung naming policy %s", r.Refused, code, rung, policyID)
	}
}

func hasIssue(v *engine.View, ref engine.Ref, code string) bool {
	for _, is := range v.Issues(ref) {
		if is.Code == code {
			return true
		}
	}
	return false
}

// tokenCaller is the lane a relay-minted token of the user carries inside the project: the user's member account there.
func tokenCaller(t *testing.T, plans *plantest.Plans, userID, projectID string) *plan.CallerPlan {
	t.Helper()
	cp, refused := plans.Root().MemberLane(userID, projectID)
	if refused != nil {
		t.Fatalf("no lane for user %s in project %s: %v", userID, projectID, refused)
	}
	return cp
}

func keyCaller(t *testing.T, plans *plantest.Plans, k *key.Key) *plan.CallerPlan {
	t.Helper()
	kp := plans.Root().Keys.Lookup(k.Spec.KeyHash, time.Now())
	if kp == nil {
		t.Fatalf("no plan for key %s", k.Meta.Name)
	}
	return kp.Caller
}

func off() *bool { b := false; return &b }
