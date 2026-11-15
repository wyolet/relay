package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/auth/apikey"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
)

// scopingAuthz is an Authorizer and Scoper that hides rows owned by hiddenOwner.
type scopingAuthz struct {
	authz.AlwaysAllowAuthenticated
	hiddenOwner meta.Owner
}

func (s scopingAuthz) Visible(_ context.Context, _, _ string, owner meta.Owner) bool {
	return owner != s.hiddenOwner
}

// orphanKey is a user's key whose account does not exist, which the engine reports as ref_missing on the key.
func orphanKey(name string) *key.Key {
	uid := meta.NewID()
	return &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerUser, ID: uid}},
		Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalUser, ID: uid}, AccountID: meta.NewID(), KeyHash: apikey.Hash(name)},
	}
}

// refMissing is the issue an orphan key carries, as the endpoint renders it for a caller who may see the key.
func refMissing(k *key.Key) PlanIssue {
	return PlanIssue{
		Code: plan.IssueRefMissing, Message: "names an account that was deleted; the relay key cannot serve",
		Kind: "key", ID: k.Meta.ID, Name: k.Meta.Name, Field: "spec.accountId", Severity: "error",
		Refs: []PlanIssueRef{{Kind: "account", ID: k.Spec.AccountID}},
	}
}

// issuesDeps mounts the world's view: two orphan keys of different users, and a policy linking a disabled rate limit a team owns.
func issuesDeps(t *testing.T) (d Deps, k1, k2 *key.Key, pol *policy.Policy, rl *ratelimit.RateLimit) {
	t.Helper()
	w := &plantest.World{}
	k1, k2 = orphanKey("orphan-a"), orphanKey("orphan-b")
	w.Keys = append(w.Keys, k1, k2)
	rl = w.RateLimit(plantest.Rule(ratelimit.MeterRequests, 10, time.Minute))
	off := false
	rl.Spec.Enabled = &off
	rl.Meta.Owner = meta.Owner{Kind: meta.OwnerTeam, ID: meta.NewID()}
	pol = w.Policy("limited", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: rl.Meta.ID}}})
	d = mountDeps(t)
	d.PlanView = w.Load(t).Engine.View
	return d, k1, k2, pol, rl
}

// getIssues asks GET /plan-issues as who ("" for no credential) and decodes a 200.
func getIssues(t *testing.T, d Deps, who, query string) (int, []PlanIssue) {
	t.Helper()
	r := chi.NewRouter()
	Mount(r, d)
	req := httptest.NewRequest(http.MethodGet, "/plan-issues"+query, nil)
	if who != "" {
		req.Header.Set("Authorization", "Bearer "+who)
	}
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

// Every issue is rendered whole, sorted by kind, name and code, and each filter narrows the list; filters combine.
func TestPlanIssues(t *testing.T) {
	d, k1, k2, pol, rl := issuesDeps(t)
	onPolicy := PlanIssue{
		Code: plan.IssueRateLimitDisabled, Message: "rate limit " + rl.Meta.Name + " is disabled; it does not apply to this policy",
		Kind: "policy", ID: pol.Meta.ID, Name: pol.Meta.Name, Field: "spec.rateLimits[0]", Severity: "warning",
		Refs: []PlanIssueRef{{Kind: "ratelimit", ID: rl.Meta.ID, Name: rl.Meta.Name}},
	}
	onK1, onK2 := refMissing(k1), refMissing(k2)
	rlRef, polRef := "ratelimit:"+rl.Meta.ID, "policy:"+pol.Meta.ID

	for _, tc := range []struct {
		query string
		want  []PlanIssue
	}{
		{"", []PlanIssue{onK1, onK2, onPolicy}},
		{"?kind=key", []PlanIssue{onK1, onK2}},
		{"?kind=policy", []PlanIssue{onPolicy}},
		{"?kind=pool", []PlanIssue{}},
		{"?kind=key&id=" + k2.Meta.ID, []PlanIssue{onK2}},
		{"?kind=policy&id=" + k2.Meta.ID, []PlanIssue{}},
		// id alone names no object, so it filters nothing.
		{"?id=" + k2.Meta.ID, []PlanIssue{onK1, onK2, onPolicy}},
		// target keeps the issues naming an object; about adds the object's own.
		{"?target=" + rlRef, []PlanIssue{onPolicy}},
		{"?target=" + polRef, []PlanIssue{}},
		{"?target=account:" + k1.Spec.AccountID, []PlanIssue{onK1}},
		{"?target=" + rlRef + "&kind=key", []PlanIssue{}},
		{"?about=" + rlRef, []PlanIssue{onPolicy}},
		{"?about=" + polRef, []PlanIssue{onPolicy}},
		{"?about=" + polRef + "&about=key:" + k1.Meta.ID, []PlanIssue{onK1, onPolicy}},
		{"?about=" + rlRef + "&kind=policy&id=" + pol.Meta.ID, []PlanIssue{onPolicy}},
		{"?about=ratelimit:" + meta.NewID(), []PlanIssue{}},
	} {
		code, got := getIssues(t, d, d.AdminToken, tc.query)
		if diff := cmp.Diff(tc.want, got); code != http.StatusOK || diff != "" {
			t.Errorf("%s: status %d, issues (-want +got):\n%s", tc.query, code, diff)
		}
	}
	for _, bad := range []string{"?target=" + rl.Meta.ID, "?target=a:b:c", "?about=" + rl.Meta.ID, "?about=" + polRef + "&about=:x", "?about=policy:a:b"} {
		if code, _ := getIssues(t, d, d.AdminToken, bad); code != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d, want 422", bad, code)
		}
	}
	if code, _ := getIssues(t, d, "", ""); code != http.StatusUnauthorized {
		t.Errorf("no credential = %d, want 401", code)
	}
}

// With no plan engine, or nothing wrong, the list is empty rather than null or an error.
func TestPlanIssues_NoView(t *testing.T) {
	d := mountDeps(t)
	for name, view := range map[string]func() Deps{
		"no engine":     func() Deps { d.PlanView = nil; return d },
		"nothing wrong": func() Deps { d.PlanView = (&plantest.World{}).Load(t).Engine.View; return d },
	} {
		code, got := getIssues(t, view(), d.AdminToken, "")
		if code != http.StatusOK || got == nil || len(got) != 0 {
			t.Errorf("%s: %d %v, want 200 with an empty list", name, code, got)
		}
	}
}

// Under a scoping authorizer a caller gets the issues of the rows it may read, and a ref it may not read keeps its kind alone.
func TestPlanIssues_ScopedToVisibleRows(t *testing.T) {
	d, k1, k2, pol, rl := issuesDeps(t)

	d.Authz = scopingAuthz{hiddenOwner: k2.Meta.Owner}
	_, got := getIssues(t, d, d.AdminToken, "?kind=key")
	if diff := cmp.Diff([]PlanIssue{refMissing(k1)}, got); diff != "" {
		t.Errorf("with one key hidden (-want +got):\n%s", diff)
	}

	d.Authz = scopingAuthz{hiddenOwner: rl.Meta.Owner}
	_, got = getIssues(t, d, d.AdminToken, "?kind=policy&id="+pol.Meta.ID)
	if len(got) != 1 || len(got[0].Refs) != 1 || got[0].Refs[0] != (PlanIssueRef{Kind: "ratelimit"}) {
		t.Errorf("with the rate limit hidden: %+v, want the policy's issue with its ref's id and name cleared", got)
	}
}

// An issue on a derived object has no owner to scope by: every caller sees it when nothing scopes, and only an admin when something does.
func TestIssueVisible_DerivedKind(t *testing.T) {
	user := actor.WithActor(context.Background(), &actor.Actor{UserID: meta.NewID(), Username: "user"})
	admin := actor.WithActor(context.Background(), &actor.Actor{AdminToken: true, Username: "admin-token"})
	for _, tc := range []struct {
		name string
		ctx  context.Context
		az   authz.Authorizer
		want bool
	}{
		{"unscoped, user", user, authz.AlwaysAllowAuthenticated{}, true},
		{"unscoped, admin", admin, authz.AlwaysAllowAuthenticated{}, true},
		{"scoped, user", user, scopingAuthz{}, false},
		{"scoped, admin", admin, scopingAuthz{}, true},
	} {
		if got := issueVisible(tc.ctx, tc.az, "", meta.NewID(), meta.Owner{}); got != tc.want {
			t.Errorf("%s: visible = %v, want %v", tc.name, got, tc.want)
		}
	}
}
