//go:build integration

package control

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

// For each target kind: an account in no binding (the service account's not made yet) gets a policy of its scope bound to it alone (none); the next call changes that policy in place (own).
func TestPseudoAccess_NoneThenOwn(t *testing.T) {
	w := newPolicyStepWorld(t)
	for _, tc := range []struct {
		kind, id string
		owner    meta.Owner
	}{
		{"team", w.teamTwo.Meta.ID, w.two},
		{"project", w.projTwo.Meta.ID, w.owner},
		{"service-account", w.sa.Meta.ID, w.owner},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			w.loadPlans(t)
			rec, first := w.access(t, "bob", target(tc.kind, tc.id), map[string]any{"accountIds": []string{w.teamTwoAcct.Meta.ID}, "models": []string{"acme/m1"}}, false)
			wantStatus(t, rec, http.StatusCreated)
			if first.Case != pseudo.CaseNone || first.Policy == nil || len(first.Pools) != 1 {
				t.Fatalf("first answer = %+v", first)
			}
			var accountID string
			for _, r := range first.Created {
				if r.Kind == "pool" {
					p, err := w.stores.Pool.Get(t.Context(), r.ID)
					must(t, "pool", err)
					accountID = p.Meta.Labels[pseudo.AccessLabel]
					if p.Meta.Owner != tc.owner || !slices.Equal(p.Spec.AccountIDs, []string{w.teamTwoAcct.Meta.ID}) {
						t.Errorf("pool = %+v", p)
					}
				}
			}
			pol := w.heldPolicy(t, accountID)
			if pol == nil || pol.Meta.ID != first.Policy.ID || pol.Meta.Owner != tc.owner || !slices.Equal(pol.Spec.PoolIDs, []string{first.Pools[0].ID}) || !slices.Equal(pol.Spec.Models, []string{"acme/m1"}) {
				t.Fatalf("account %s held under %+v", accountID, pol)
			}

			w.loadPlans(t)
			rec, second := w.access(t, "bob", target(tc.kind, tc.id), map[string]any{"accountIds": []string{w.teamTwoAcct.Meta.ID}, "models": []string{"acme"}}, false)
			wantStatus(t, rec, http.StatusCreated)
			want := []CreatedRow{{Kind: "policy", ID: pol.Meta.ID, Name: pol.Meta.Name}}
			if diff := cmp.Diff(want, second.Updated); diff != "" || second.Case != pseudo.CaseOwn || len(second.Created) != 0 || len(second.Deleted) != 0 {
				t.Errorf("second answer %+v; updated (-want +got):\n%s", second, diff)
			}
			if after := w.heldPolicy(t, accountID); !slices.Equal(after.Spec.Models, []string{"acme"}) || !slices.Equal(after.Spec.PoolIDs, pol.Spec.PoolIDs) {
				t.Errorf("policy after = %+v", after.Spec)
			}
		})
	}
}

// The access is stated whole: pools are added, reordered and dropped; the labelled pool of a host is reused, and deleted once the policy no longer links it; an unlabelled pool is linked and unlinked, never changed; the rate limit link stays.
func TestPseudoAccess_AddRemoveReorder(t *testing.T) {
	w := newPolicyStepWorld(t)
	tgt := target("project", w.projTwo.Meta.ID)
	keyless := w.keylessPool
	keylessBefore := mustJSON(t, w.storedPool(t, keyless.Meta.ID))
	w.loadPlans(t)
	rec, first := w.access(t, "bob", tgt, map[string]any{"accountIds": []string{w.teamTwoAcct.Meta.ID, w.secondHostAcct.Meta.ID}}, false)
	wantStatus(t, rec, http.StatusCreated)
	if len(first.Pools) != 2 {
		t.Fatalf("first = %+v", first)
	}
	hostA, hostB := first.Pools[0], first.Pools[1]
	pol := w.heldPolicy(t, w.projTwoOwn.Meta.ID)
	rl := w.rateLimitFor(t, w.owner)
	pol.Spec.RateLimits = []policy.RateLimitLink{{RateLimitID: rl}}
	must(t, "link a rate limit", w.stores.Policy.Upsert(t.Context(), pol))

	w.loadPlans(t)
	rec, second := w.access(t, "bob", tgt, map[string]any{"poolIds": []string{keyless.Meta.ID}, "accountIds": []string{w.teamTwoAcct.Meta.ID}}, false)
	wantStatus(t, rec, http.StatusCreated)
	if diff := cmp.Diff([]RowRef{{ID: keyless.Meta.ID, Name: keyless.Meta.Name}, hostA}, second.Pools); diff != "" {
		t.Errorf("pools (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]CreatedRow{{Kind: "pool", ID: hostB.ID, Name: hostB.Name}}, second.Deleted); diff != "" {
		t.Errorf("deleted (-want +got):\n%s", diff)
	}
	if got := w.heldPolicy(t, w.projTwoOwn.Meta.ID); len(got.Spec.RateLimits) != 1 || got.Spec.RateLimits[0].RateLimitID != rl {
		t.Errorf("rate limit links = %+v, want the one linked kept", got.Spec.RateLimits)
	}

	w.loadPlans(t)
	rec, third := w.access(t, "bob", tgt, map[string]any{"accountIds": []string{w.secondHostAcct.Meta.ID, w.teamTwoAcct.Meta.ID}}, false)
	wantStatus(t, rec, http.StatusCreated)
	if len(third.Pools) != 2 || third.Pools[1] != hostA {
		t.Errorf("third pools = %+v, want a new pool at the second host, then %v", third.Pools, hostA)
	}

	w.loadPlans(t)
	rec, fourth := w.access(t, "bob", tgt, map[string]any{"everyCredential": true}, false)
	wantStatus(t, rec, http.StatusCreated)
	if len(fourth.Pools) != 0 || len(fourth.Deleted) != 2 {
		t.Errorf("every credential = %+v, want no pools and both labelled pools deleted", fourth)
	}
	if diff := cmp.Diff(keylessBefore, mustJSON(t, w.storedPool(t, keyless.Meta.ID))); diff != "" {
		t.Errorf("the unlabelled pool changed (-before +after):\n%s", diff)
	}
}

// A dry run answers what would be written and writes nothing; without access it answers only the case and what the caller may see of the current policy, a hidden pool counted.
func TestPseudoAccess_DryRunAndPrefill(t *testing.T) {
	w := newPolicyStepWorld(t)
	tgt := target("project", w.projTwo.Meta.ID)
	hidden := w.hiddenPool(t)
	own := w.policy(t, "two-app-own", w.owner, policy.Spec{Models: []string{"acme/m1"}, PoolIDs: []string{w.twoPool.Meta.ID, hidden.Meta.ID}})
	w.bind(t, own, w.owner, w.projTwoOwn)
	w.loadPlans(t)
	before := w.rowCounts(t)

	rec, pre := w.access(t, "bob", tgt, nil, true)
	wantStatus(t, rec, http.StatusOK)
	want := PseudoAccessAnswer{Case: pseudo.CaseOwn, Warnings: []PseudoIssue{}, Current: &PolicyCurrent{
		Policy: RowRef{ID: own.Meta.ID, Name: own.Meta.Name}, HiddenPools: 1, Models: []string{"acme/m1"}, BindingEnabled: true,
		Pools: []CurrentPool{{ID: w.twoPool.Meta.ID, Name: w.twoPool.Meta.Name, Host: RowRef{ID: w.host.Meta.ID, Name: w.host.Meta.Name}, Accounts: []RowRef{{ID: w.teamTwoAcct.Meta.ID, Name: w.teamTwoAcct.Meta.Name}}}},
	}}
	if diff := cmp.Diff(want, pre); diff != "" {
		t.Errorf("prefill (-want +got):\n%s", diff)
	}
	if strings.Contains(rec.Body.String(), hidden.Meta.ID) || strings.Contains(rec.Body.String(), hidden.Meta.Name) {
		t.Errorf("the prefill names the hidden pool: %s", rec.Body)
	}

	rec, dry := w.access(t, "bob", tgt, map[string]any{"accountIds": []string{w.secondHostAcct.Meta.ID}}, true)
	wantStatus(t, rec, http.StatusOK)
	if dry.Case != pseudo.CaseOwn || len(dry.Created) != 1 || len(dry.Updated) != 1 {
		t.Errorf("dry run = %+v", dry)
	}
	if diff := cmp.Diff(before, w.rowCounts(t)); diff != "" {
		t.Errorf("a dry run wrote rows (-before +after):\n%s", diff)
	}
	if got := w.stored(t, own); got.Meta.ResourceVersion != own.Meta.ResourceVersion {
		t.Errorf("a dry run updated the policy: %s → %s", own.Meta.ResourceVersion, got.Meta.ResourceVersion)
	}
}

// A late refusal writes nothing of the rows before it: the binding's grant refused, the pool and policy made before it are gone.
func TestPseudoAccess_LateRefusalWritesNothing(t *testing.T) {
	w := newPolicyStepWorld(t)
	w.loadPlans(t)
	before := w.rowCounts(t)
	*w.denied = "policy-bindings.create"
	rec, _ := w.access(t, "bob", target("project", w.projTwo.Meta.ID), map[string]any{"accountIds": []string{w.teamTwoAcct.Meta.ID}}, false)
	*w.denied = ""
	if rec.Code != http.StatusForbidden || refusalOf(t, rec).Pointer != "/spec/access" {
		t.Fatalf("status %d %s, want 403 at /spec/access", rec.Code, rec.Body)
	}
	if diff := cmp.Diff(before, w.rowCounts(t)); diff != "" {
		t.Errorf("a refused request wrote rows (-before +after):\n%s", diff)
	}
}

// A policy another account shares is copied, not edited: the other account's binding, policy, pools and plan stay byte for byte what they were.
func TestPseudoAccess_SharedPolicyLeavesTheOtherAccount(t *testing.T) {
	w := newPolicyStepWorld(t)
	shared := w.policy(t, "two-shared", w.two, policy.Spec{PoolIDs: []string{w.twoPool.Meta.ID}})
	held := w.bind(t, shared, w.two, w.teamTwoOwn, w.projTwoOwn)
	w.loadPlans(t)
	other := w.teamTwoOwn.Meta.ID
	reachBefore := w.reach(t, other)
	if !strings.Contains(reachBefore, w.m1.Meta.ID) {
		t.Fatalf("the other account reaches nothing to compare: %s", reachBefore)
	}
	poolBefore := mustJSON(t, w.storedPool(t, w.twoPool.Meta.ID))

	rec, out := w.access(t, "bob", target("project", w.projTwo.Meta.ID), map[string]any{"accountIds": []string{w.secondHostAcct.Meta.ID}}, false)
	wantStatus(t, rec, http.StatusCreated)
	if out.Case != pseudo.CaseCopy {
		t.Fatalf("case %s, want copy", out.Case)
	}
	if diff := cmp.Diff(mustJSON(t, shared), mustJSON(t, w.stored(t, shared))); diff != "" {
		t.Errorf("the shared policy changed (-before +after):\n%s", diff)
	}
	if diff := cmp.Diff(poolBefore, mustJSON(t, w.storedPool(t, w.twoPool.Meta.ID))); diff != "" {
		t.Errorf("its pool changed (-before +after):\n%s", diff)
	}
	if b := w.holdingOf(t, other); b == nil || b.Meta.ID != held.Meta.ID || !slices.Equal(b.Spec.AccountIDs, []string{other}) {
		t.Errorf("the other account is held by %+v", b)
	}
	w.loadPlans(t)
	if diff := cmp.Diff(reachBefore, w.reach(t, other)); diff != "" {
		t.Errorf("the other account's plan changed (-before +after):\n%s", diff)
	}
	if p := w.heldPolicy(t, w.projTwoOwn.Meta.ID); p == nil || p.Meta.Owner != w.owner || p.Meta.ID == shared.Meta.ID {
		t.Errorf("the target is held under %+v, want a copy owned by its project", p)
	}
}

// A copy refuses to carry a link its scope may not link (here a budget of another team), naming the count and kind only; the access kind, which keeps a policy's budgets, refuses it too.
func TestPseudoAccess_UncarriedLinks(t *testing.T) {
	w := newPolicyStepWorld(t)
	other := &budget.Budget{Meta: meta.Metadata{ID: ids.New(), Name: "one-budget", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: w.teamOne.Meta.ID}}, Spec: budget.Spec{AmountUSD: "10"}}
	other.Default()
	must(t, "budget", w.stores.Budget.Upsert(t.Context(), other))
	shared := w.policy(t, "two-shared", w.two, policy.Spec{PoolIDs: []string{w.twoPool.Meta.ID}, Budgets: []policy.BudgetLink{{BudgetID: other.Meta.ID}}})
	w.bind(t, shared, w.two, w.teamTwoOwn, w.projTwoOwn)
	w.loadPlans(t)
	before := w.rowCounts(t)
	tgt := target("project", w.projTwo.Meta.ID)

	for name, send := range map[string]func() *httptest.ResponseRecorder{
		"limits": func() *httptest.ResponseRecorder { rec, _ := w.limits(t, "bob", tgt, aBudget, false); return rec },
		"access": func() *httptest.ResponseRecorder {
			rec, _ := w.access(t, "bob", tgt, map[string]any{"poolIds": []string{w.twoPool.Meta.ID}}, false)
			return rec
		},
	} {
		rec := send()
		if rec.Code != http.StatusBadRequest || errorCode(t, rec) != codeLinksNotCarried || !strings.Contains(rec.Body.String(), "1 budget") {
			t.Errorf("%s: %d %s, want 400 links_not_carried naming 1 budget", name, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), other.Meta.ID) || strings.Contains(rec.Body.String(), other.Meta.Name) {
			t.Errorf("%s: the refusal names the budget: %s", name, rec.Body)
		}
	}
	if diff := cmp.Diff(before, w.rowCounts(t)); diff != "" {
		t.Errorf("a refused request wrote rows (-before +after):\n%s", diff)
	}
}

// A pool link the caller may not see is kept at its place, in place and in a copy, and the answer counts it without naming it; a limit on a copy carries it too.
func TestPseudoAccess_KeepsHiddenPools(t *testing.T) {
	for _, c := range []pseudo.PolicyCase{pseudo.CaseOwn, pseudo.CaseCopy} {
		t.Run(string(c), func(t *testing.T) {
			w := newPolicyStepWorld(t)
			hidden := w.hiddenPool(t)
			links := policy.Spec{PoolIDs: []string{hidden.Meta.ID, w.twoPool.Meta.ID}}
			if c == pseudo.CaseOwn {
				w.bind(t, w.policy(t, "two-app-own", w.owner, links), w.owner, w.projTwoOwn)
			} else {
				w.bind(t, w.policy(t, "two-shared", w.two, links), w.two, w.teamTwoOwn, w.projTwoOwn)
			}
			w.loadPlans(t)
			rec, out := w.access(t, "bob", target("project", w.projTwo.Meta.ID), map[string]any{"accountIds": []string{w.secondHostAcct.Meta.ID}}, false)
			wantStatus(t, rec, http.StatusCreated)
			if out.Case != c || out.HiddenPoolsKept != 1 || len(out.Pools) != 1 {
				t.Fatalf("answer = %+v, want %s keeping 1 hidden pool and naming the one made", out, c)
			}
			if strings.Contains(rec.Body.String(), hidden.Meta.ID) || strings.Contains(rec.Body.String(), hidden.Meta.Name) {
				t.Errorf("the answer names the hidden pool: %s", rec.Body)
			}
			got := w.heldPolicy(t, w.projTwoOwn.Meta.ID)
			if diff := cmp.Diff([]string{hidden.Meta.ID, out.Pools[0].ID}, got.Spec.PoolIDs); diff != "" {
				t.Errorf("policy pools (-want +got):\n%s", diff)
			}
		})
	}
}

// A concurrent change to the policy between the request's read and its write answers 409 conflict and writes nothing.
func TestPseudoAccess_ConcurrentChange(t *testing.T) {
	w := newPolicyStepWorld(t)
	own := w.policy(t, "two-app-own", w.owner, policy.Spec{PoolIDs: []string{w.twoPool.Meta.ID}})
	w.bind(t, own, w.owner, w.projTwoOwn)
	w.loadPlans(t)
	fired := false
	w.before = func(action string) {
		if action != "policies.update" || fired {
			return
		}
		fired = true
		changed := *w.stored(t, own)
		changed.Spec.Models = []string{"acme/m2"}
		must(t, "concurrent change", w.stores.Policy.Upsert(t.Context(), &changed))
	}
	rec, _ := w.access(t, "bob", target("project", w.projTwo.Meta.ID), map[string]any{"accountIds": []string{w.teamTwoAcct.Meta.ID}, "models": []string{"acme/m1"}}, false)
	w.before = nil
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "conflict" {
		t.Fatalf("status %d %s, want 409 conflict", rec.Code, rec.Body)
	}
	if got := w.stored(t, own); !slices.Equal(got.Spec.Models, []string{"acme/m2"}) || len(w.labelledOf(t, w.projTwoOwn.Meta.ID)) != 0 {
		t.Errorf("policy %+v after the refused request, labelled pools %d", got.Spec, len(w.labelledOf(t, w.projTwoOwn.Meta.ID)))
	}
}

// Each step is audited as its own route records it, the policy's update as an update and the labelled pool's delete as a delete; the prefill, a dry run, is not audited.
func TestPseudoAccess_Audit(t *testing.T) {
	sink := &auditSink{}
	em := audit.NewEmitter(sink, slog.New(slog.DiscardHandler))
	w := newPolicyStepWorld(t, func(d *Deps) { d.Audit = em })
	labelled := &pool.Pool{Meta: meta.Metadata{ID: ids.New(), Name: "two-app-labelled", Owner: w.owner, Labels: map[string]string{pseudo.AccessLabel: w.projTwoOwn.Meta.ID}}, Spec: pool.Spec{HostID: w.host.Meta.ID, AccountIDs: []string{w.teamTwoAcct.Meta.ID}}}
	labelled.Default()
	must(t, "labelled pool", w.stores.Pool.Upsert(t.Context(), labelled))
	own := w.policy(t, "two-app-own", w.owner, policy.Spec{PoolIDs: []string{labelled.Meta.ID}})
	w.bind(t, own, w.owner, w.projTwoOwn)
	w.loadPlans(t)
	tgt := target("project", w.projTwo.Meta.ID)

	rec, _ := w.access(t, "bob", tgt, nil, true)
	wantStatus(t, rec, http.StatusOK)
	rec, _ = w.access(t, "bob", tgt, map[string]any{"everyCredential": true}, false)
	wantStatus(t, rec, http.StatusCreated)
	em.Close()
	type entry struct{ Action, ID, Status string }
	var got []entry
	for _, ev := range sink.all() {
		if strings.HasSuffix(ev.Request.Path, "/pseudo-access") {
			got = append(got, entry{ev.Action, ev.Resource.ID, ev.Outcome.Status})
		}
	}
	want := []entry{{"policies.update", own.Meta.ID, audit.StatusAllowed}, {"pools.delete", labelled.Meta.ID, audit.StatusAllowed}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("audit (-want +got):\n%s", diff)
	}
}

// With everyCredential and no models, an account in no binding needs nothing written, and the answer says so.
func TestPseudoAccess_NothingToWrite(t *testing.T) {
	w := newPolicyStepWorld(t)
	w.loadPlans(t)
	before := w.rowCounts(t)
	rec, out := w.access(t, "bob", target("team", w.teamTwo.Meta.ID), map[string]any{"everyCredential": true}, false)
	wantStatus(t, rec, http.StatusOK)
	want := PseudoAccessAnswer{Case: pseudo.CaseNone, Unchanged: true, Warnings: []PseudoIssue{}}
	if diff := cmp.Diff(want, out); diff != "" {
		t.Errorf("answer (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(before, w.rowCounts(t)); diff != "" {
		t.Errorf("rows written (-before +after):\n%s", diff)
	}
}
