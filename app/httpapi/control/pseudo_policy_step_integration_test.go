//go:build integration

package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/pricing"
	"github.com/wyolet/relay/pkg/catalogkind/provider"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

// policyStepWorld adds to the agent world every pseudo kind served, a catalog at its host (provider acme, model m1 priced there and m2 not), a pool of team two drawing on its provider account, and team two's and project two's own accounts, both in no binding. bob administers team two; the service account two-bot has no account yet.
type policyStepWorld struct {
	*pseudoAgentWorld
	m1, m2                 *model.Model
	twoPool                *pool.Pool
	teamTwoOwn, projTwoOwn *account.Account
	two                    meta.Owner
}

func newPolicyStepWorld(t *testing.T, opts ...func(*Deps)) *policyStepWorld {
	t.Helper()
	w := &policyStepWorld{pseudoAgentWorld: newPseudoAgentWorld(t, append([]func(*Deps){func(d *Deps) { d.PseudoKinds = []string{"all"} }}, opts...)...)}
	ctx, s := t.Context(), w.stores
	system := meta.Owner{Kind: meta.OwnerSystem}
	w.two = meta.Owner{Kind: meta.OwnerTeam, ID: w.teamTwo.Meta.ID}
	prov := &provider.Provider{Meta: meta.Metadata{ID: ids.New(), Name: "acme", Owner: system}}
	must(t, "provider", s.Provider.Upsert(ctx, prov))
	mk := func(name string) *model.Model {
		m := &model.Model{Meta: meta.Metadata{ID: ids.New(), Name: name, Owner: meta.Owner{Kind: meta.OwnerProvider, ID: prov.Meta.ID}}, Spec: model.Spec{Snapshots: []model.Snapshot{{Name: name}}, Pointer: name}}
		must(t, "model "+name, s.Model.Upsert(ctx, m))
		return m
	}
	w.m1, w.m2 = mk("m1"), mk("m2")
	pr := &pricing.Pricing{
		Meta: meta.Metadata{ID: ids.New(), Name: "m1-price", Owner: meta.Owner{Kind: meta.OwnerHost, ID: w.host.Meta.ID}},
		Spec: pricing.Spec{Currency: "USD", TargetModelIDs: []string{w.m1.Meta.ID}, Rates: []pricing.Rate{{Meter: pricing.MeterTokensInput, Unit: pricing.UnitPerMillion, Amount: 1}}},
	}
	must(t, "pricing", s.Pricing.Upsert(ctx, pr))
	for _, b := range []*binding.Binding{
		{Meta: meta.Metadata{ID: ids.New(), Name: "m1-at-share-host", Owner: system}, Spec: binding.Spec{ModelID: w.m1.Meta.ID, HostID: w.host.Meta.ID, Adapter: adapters.OpenAI, PricingID: pr.Meta.ID}},
		{Meta: meta.Metadata{ID: ids.New(), Name: "m2-at-share-host", Owner: system}, Spec: binding.Spec{ModelID: w.m2.Meta.ID, HostID: w.host.Meta.ID, Adapter: adapters.OpenAI}},
	} {
		must(t, "host binding", s.Binding.Upsert(ctx, b))
	}
	w.twoPool = &pool.Pool{Meta: meta.Metadata{ID: ids.New(), Name: "two-pool", Owner: w.two}, Spec: pool.Spec{HostID: w.host.Meta.ID, AccountIDs: []string{w.teamTwoAcct.Meta.ID}}}
	w.twoPool.Default()
	must(t, "pool", s.Pool.Upsert(ctx, w.twoPool))
	w.teamTwoOwn = w.managed(t, "team:"+w.teamTwo.Meta.ID)
	w.projTwoOwn = w.managed(t, "project:"+w.projTwo.Meta.ID)
	must(t, "reload", w.cat.Reload(ctx))
	return w
}

// managed is the stored caller account of the managed key; nil when there is none.
func (w *policyStepWorld) managed(t *testing.T, key string) *account.Account {
	t.Helper()
	all, err := w.stores.Account.List(t.Context())
	must(t, "accounts", err)
	for _, a := range all {
		if a.ManagedKey() == key {
			return a
		}
	}
	return nil
}

func (w *policyStepWorld) policy(t *testing.T, name string, owner meta.Owner, spec policy.Spec) *policy.Policy {
	t.Helper()
	p := &policy.Policy{Meta: meta.Metadata{ID: ids.New(), Name: name, Owner: owner}, Spec: spec}
	must(t, "policy "+name, w.stores.Policy.Upsert(t.Context(), p))
	return w.stored(t, p)
}

func (w *policyStepWorld) stored(t *testing.T, p *policy.Policy) *policy.Policy {
	t.Helper()
	got, err := w.stores.Policy.Get(t.Context(), p.Meta.ID)
	must(t, "policy", err)
	return got
}

func (w *policyStepWorld) bind(t *testing.T, p *policy.Policy, owner meta.Owner, accounts ...*account.Account) *policybinding.PolicyBinding {
	t.Helper()
	b := &policybinding.PolicyBinding{Meta: meta.Metadata{ID: ids.New(), Name: p.Meta.Name + "-binding-" + ids.New()[30:], Owner: owner}, Spec: policybinding.Spec{PolicyID: p.Meta.ID}}
	for _, a := range accounts {
		b.Spec.AccountIDs = append(b.Spec.AccountIDs, a.Meta.ID)
	}
	must(t, "binding", w.stores.PolicyBinding.Upsert(t.Context(), b))
	return b
}

func target(kind, id string) map[string]string { return map[string]string{"kind": kind, "id": id} }

// postPseudo posts body to path as who and decodes a success into out.
func (w *policyStepWorld) postPseudo(t *testing.T, who, path string, body map[string]any, dryRun bool, out any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	must(t, "body", err)
	if dryRun {
		path += "?dryRun=true"
	}
	rec := w.send(who, http.MethodPost, path, string(raw))
	if rec.Code < 300 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Errorf("decode %s: %v", rec.Body, err)
		}
	}
	return rec
}

func (w *policyStepWorld) access(t *testing.T, who string, tgt map[string]string, access map[string]any, dryRun bool) (*httptest.ResponseRecorder, PseudoAccessAnswer) {
	t.Helper()
	spec := map[string]any{"target": tgt}
	if access != nil {
		spec["access"] = access
	}
	var out PseudoAccessAnswer
	rec := w.postPseudo(t, who, "/pseudo-access", map[string]any{"spec": spec}, dryRun, &out)
	return rec, out
}

func (w *policyStepWorld) limits(t *testing.T, who string, tgt map[string]string, limit map[string]any, dryRun bool) (*httptest.ResponseRecorder, PseudoLimitsAnswer) {
	t.Helper()
	spec := map[string]any{"target": tgt}
	for k, v := range limit {
		spec[k] = v
	}
	var out PseudoLimitsAnswer
	rec := w.postPseudo(t, who, "/pseudo-limits", map[string]any{"metadata": map[string]any{"displayName": "CI cap"}, "spec": spec}, dryRun, &out)
	return rec, out
}

// reach is account id's reach in the current plans, as JSON.
func (w *policyStepWorld) reach(t *testing.T, id string) string {
	t.Helper()
	raw, err := json.Marshal(plan.ReachOf(w.plans.View(), id, nil))
	must(t, "reach", err)
	return string(raw)
}

// heldPolicy is the policy the binding holding the account uses; nil when none holds it.
func (w *policyStepWorld) heldPolicy(t *testing.T, accountID string) *policy.Policy {
	t.Helper()
	b := w.holdingOf(t, accountID)
	if b == nil {
		return nil
	}
	p, err := w.stores.Policy.Get(t.Context(), b.Spec.PolicyID)
	must(t, "policy", err)
	return p
}

func (w *policyStepWorld) storedPool(t *testing.T, id string) *pool.Pool {
	t.Helper()
	p, err := w.stores.Pool.Get(t.Context(), id)
	must(t, "pool", err)
	return p
}

// hiddenPool is a pool of team two at the host that the caller may not see.
func (w *policyStepWorld) hiddenPool(t *testing.T) *pool.Pool {
	t.Helper()
	p := &pool.Pool{Meta: meta.Metadata{ID: ids.New(), Name: "two-hidden-pool", Owner: w.two}, Spec: pool.Spec{HostID: w.host.Meta.ID, AccountIDs: []string{w.teamTwoAcct.Meta.ID}}}
	p.Default()
	must(t, "hidden pool", w.stores.Pool.Upsert(t.Context(), p))
	w.hidden[p.Meta.ID] = true
	return p
}

// labelledOf lists the pools labelled for the account.
func (w *policyStepWorld) labelledOf(t *testing.T, accountID string) []*pool.Pool {
	t.Helper()
	all, err := w.stores.Pool.List(t.Context())
	must(t, "pools", err)
	var out []*pool.Pool
	for _, p := range all {
		if p.Meta.Labels[pseudo.AccessLabel] == accountID {
			out = append(out, p)
		}
	}
	return out
}

// rateLimitFor makes a rate limit owned by owner and returns its id.
func (w *policyStepWorld) rateLimitFor(t *testing.T, owner meta.Owner) string {
	t.Helper()
	rl := &ratelimit.RateLimit{Meta: meta.Metadata{ID: ids.New(), Name: "limit-" + ids.New()[30:], Owner: owner}, Spec: ratelimit.Spec{Rules: []ratelimit.Rule{{Meter: ratelimit.MeterRequests, Amount: 100, Window: ratelimit.Window(time.Minute), Strategy: "sliding-window"}}}}
	must(t, "rate limit", w.stores.RateLimit.Upsert(t.Context(), rl))
	return rl.Meta.ID
}
