package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/ratelimit"
)

// disabledLimitDeps mounts a view in which a policy links a disabled rate limit: one issue on the policy, naming the rate limit.
func disabledLimitDeps(t *testing.T) (Deps, *policy.Policy, *ratelimit.RateLimit) {
	t.Helper()
	w := &plantest.World{}
	rl := w.RateLimit(plantest.Rule(ratelimit.MeterRequests, 10, time.Minute))
	off := false
	rl.Spec.Enabled = &off
	rl.Meta.Owner = meta.Owner{Kind: meta.OwnerTeam, ID: meta.NewID()}
	pol := w.Policy("limited", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: rl.Meta.ID}}})
	d := mountDeps(t)
	d.PlanView = w.Load(t).Engine.View
	return d, pol, rl
}

func getIssues(t *testing.T, d Deps, query string) (int, []PlanIssue) {
	t.Helper()
	r := chi.NewRouter()
	Mount(r, d)
	req := httptest.NewRequest(http.MethodGet, "/plan-issues"+query, nil)
	req.Header.Set("Authorization", "Bearer "+d.AdminToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		return w.Code, nil
	}
	var items []PlanIssue
	if err := json.NewDecoder(w.Body).Decode(&items); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return w.Code, items
}

// The issue carries its spec path and its ref's name.
func TestPlanIssues_FieldAndRefName(t *testing.T) {
	d, pol, rl := disabledLimitDeps(t)
	_, items := getIssues(t, d, "?kind=policy&id="+pol.Meta.ID)
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one", items)
	}
	got := items[0]
	if got.Code != plan.IssueRateLimitDisabled || got.Field != "spec.rateLimits[0]" {
		t.Errorf("code %q field %q, want %s at spec.rateLimits[0]", got.Code, got.Field, plan.IssueRateLimitDisabled)
	}
	if len(got.Refs) != 1 || got.Refs[0] != (PlanIssueRef{Kind: string(plan.KindRateLimit), ID: rl.Meta.ID, Name: rl.Meta.Name}) {
		t.Errorf("refs = %+v, want the rate limit by id and name", got.Refs)
	}

	d.Authz = scopingAuthz{hiddenOwner: rl.Meta.Owner}
	_, items = getIssues(t, d, "?kind=policy&id="+pol.Meta.ID)
	if len(items) != 1 || items[0].Refs[0].ID != "" || items[0].Refs[0].Name != "" {
		t.Errorf("hidden ref = %+v, want its id and name cleared", items)
	}
}

// target keeps the issues naming an object; about adds the object's own; both combine with kind.
func TestPlanIssues_TargetAndAbout(t *testing.T) {
	d, pol, rl := disabledLimitDeps(t)
	rlRef, polRef := "ratelimit:"+rl.Meta.ID, "policy:"+pol.Meta.ID
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"?target=" + rlRef, 1},
		{"?about=" + rlRef, 1},
		{"?about=" + polRef, 1},
		{"?target=" + polRef, 0},
		{"?target=" + rlRef + "&kind=key", 0},
		{"?about=" + rlRef + "&kind=policy&id=" + pol.Meta.ID, 1},
		{"?about=ratelimit:" + meta.NewID(), 0},
	} {
		code, items := getIssues(t, d, tc.query)
		if code != http.StatusOK || len(items) != tc.want {
			t.Errorf("%s: %d, %d items, want %d", tc.query, code, len(items), tc.want)
			continue
		}
		for _, it := range items {
			if it.ID != pol.Meta.ID {
				t.Errorf("%s: issue on %s %s, want the policy", tc.query, it.Kind, it.ID)
			}
		}
	}
	if code, _ := getIssues(t, d, "?target="+rl.Meta.ID); code != http.StatusUnprocessableEntity {
		t.Errorf("target without a kind = %d, want 422", code)
	}
}
