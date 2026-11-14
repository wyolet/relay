package inference

import (
	"github.com/wyolet/relay/app/httpapi"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/sdk/adapters/openai"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// ladderCase shapes the user's policy in ladderDeps.
type ladderCase struct {
	budget  *budget.Spec // linked when set
	models  []string
	enabled *bool
}

// ladderDeps serves "priced" (one USD per token) and "free" (no pricing) at one host answering every call with one input and one output token, so each served request costs two dollars; the user's key is governed by a policy shaped as c says. now is the limiter's clock.
func ladderDeps(t *testing.T, c ladderCase, now time.Time) (Deps, *Principal, usageCapture, *atomic.Int32, string) {
	t.Helper()
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(up.Close)

	var w plantest.World
	prov := w.Provider("openai")
	h := w.Host("openai", up.URL)
	priced, free := w.Model(prov, "priced"), w.Model(prov, "free")
	w.Bind(priced, h, adapters.OpenAI)
	w.Bind(free, h, adapters.OpenAI)
	w.Price(priced, h, 1_000_000)
	w.Credential(h, nil, plantest.System, "sk-upstream")
	u := w.User()
	k := w.UserKey(u, "rk-ladder")
	spec := policy.Spec{Models: c.models, Enabled: c.enabled}
	if c.budget != nil {
		b := w.Budget(c.budget.AmountUSD)
		b.Spec = *c.budget
		spec.Budgets = []policy.BudgetLink{{BudgetID: b.Meta.ID}}
	}
	pol := w.Policy("caller", spec)
	w.BindPolicy(pol, w.PersonalAccount(u))
	plans := w.Load(t)

	cat := catalogtest.Catalog{Providers: w.Providers, Hosts: w.Hosts, Models: w.Models, HostKeys: w.Credentials, Keys: w.Keys, Bindings: w.Bindings, Pricings: w.Pricings}.Load(t)
	d := buildRunnableDepsAt(t, cat, func() time.Time { return now })
	d.Plans = plans.Holder
	useSpecs(&d, adapter.NewRegistry(
		(&adapter.Spec{Name: adapters.OpenAI, DefaultPath: "/v1/chat/completions", Auth: adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"}, Translator: openai.CCTranslator{}, ExtractTokens: openai.ExtractTokens}).Build(),
		(&adapter.Spec{Name: adapters.Canonical, Translator: v1.IdentityTranslator{}}).Build(),
	))
	events := make(usageCapture, 256) // roomy: a full buffer would block post-flight before the charge lands
	reg := lifecycle.New()
	reg.RegisterHook(usagelog.NewUsageHook(nil, ""))
	reg.RegisterCollector(events)
	d.Lifecycle = reg
	d.Pipeline.Lifecycle = reg
	return d, principalOf(plans.Root().Keys.Lookup(k.Spec.KeyHash, time.Now()), k.Spec.KeyHash), events, &hits, pol.Meta.ID
}

func chat(d Deps, p *Principal, model string) *httptest.ResponseRecorder {
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	return dispatch(d, p, DispatchInput{Inbound: adapters.OpenAI, Body: []byte(body), ModelName: model})
}

// wantLadderRefusal checks a 403 the caller's ladder made: its code and class, and a message that names what refused (a generated row name sits between prefix and suffix) and then the lane rung and the policy.
func wantLadderRefusal(t *testing.T, w *httptest.ResponseRecorder, p *Principal, policyID, code, prefix, suffix string) {
	t.Helper()
	got := envelopeOf(t, w)
	suffix += " (lane rung account:" + p.Caller.Lane.Meta.ID + ", policy " + policyID + ")"
	if w.Code != http.StatusForbidden || got.Type != "invalid_request_error" || got.Code != code || got.Refusal != "model_not_allowed" ||
		!strings.HasPrefix(got.Message, prefix) || !strings.HasSuffix(got.Message, suffix) {
		t.Errorf("answer = %d %+v, want 403 %s with message %q…%q", w.Code, got, code, prefix, suffix)
	}
}

// A hard budget spent to its cap refuses the next request before the upstream with a 429 whose Retry-After is the next period start, naming the budget's rung and policy.
func TestBudgetRefusal_Is429WithRetryAfterAtTheNextPeriodStart(t *testing.T) {
	now := time.Date(2026, 11, 30, 23, 0, 0, 0, time.UTC) // one hour before December
	d, p, events, hits, policyID := ladderDeps(t, ladderCase{budget: &budget.Spec{AmountUSD: "1", Period: budget.PeriodMonth, Mode: budget.ModeHard}}, now)

	if w := chat(d, p, "priced"); w.Code != http.StatusOK {
		t.Fatalf("first request = %d %s", w.Code, w.Body)
	}
	events.next(t)
	// The charge lands in the detached post-flight after the usage event; a request admitted before it is in-flight overshoot, which the budget allows.
	var w *httptest.ResponseRecorder
	var served int32
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		served = hits.Load()
		if w = chat(d, p, "priced"); w.Code != http.StatusOK || time.Now().After(deadline) {
			break
		}
	}
	// The limiter's clock is fixed one hour before the month turns.
	wantAnswer(t, w, http.StatusTooManyRequests, httpapi.APIErrorBody{
		Type: "rate_limit_error", Code: "budget_exceeded", Refusal: "no_capacity",
		Message: "the budget of the lane policy " + policyID + " is exhausted; try again in 3600s",
	}, map[string]string{"Retry-After": "3600"})
	if hits.Load() != served {
		t.Fatal("the refused request reached the upstream")
	}
}

// An alert budget never refuses, however far past the cap.
func TestBudgetRefusal_AlertBudgetNeverRefuses(t *testing.T) {
	d, p, events, _, _ := ladderDeps(t, ladderCase{budget: &budget.Spec{AmountUSD: "1", Period: budget.PeriodDay, Mode: budget.ModeAlert}}, time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	for i := range 3 {
		if w := chat(d, p, "priced"); w.Code != http.StatusOK {
			t.Fatalf("request %d = %d %s", i, w.Code, w.Body)
		}
		events.next(t)
	}
}

// A model without pricing is refused under a budget before the upstream, naming the policy; a priced one is served.
func TestBudgetRefusal_UnpricedModelIsRefused(t *testing.T) {
	d, p, _, hits, policyID := ladderDeps(t, ladderCase{budget: &budget.Spec{AmountUSD: "100", Period: budget.PeriodMonth, Mode: budget.ModeHard}}, time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	w := chat(d, p, "free")
	wantLadderRefusal(t, w, p, policyID, "unpriced_under_budget", "unpriced_under_budget: binding ", " has no pricing and a budget applies")
	if hits.Load() != 0 {
		t.Fatal("the unpriced request reached the upstream")
	}
	if w := chat(d, p, "priced"); w.Code != http.StatusOK {
		t.Fatalf("priced request = %d %s", w.Code, w.Body)
	}
}

// A disabled policy refuses with a 403 (the caller is known; access is not), and the request never reaches the upstream.
func TestLadderRefusal_DisabledPolicyIsForbidden(t *testing.T) {
	disabled := false
	d, p, _, hits, policyID := ladderDeps(t, ladderCase{enabled: &disabled}, time.Now())
	w := chat(d, p, "priced")
	wantLadderRefusal(t, w, p, policyID, "policy_disabled", "policy_disabled: policy ", " is disabled")
	if hits.Load() != 0 {
		t.Fatal("a request under a disabled policy reached the upstream")
	}
}

// A model outside the grant is refused naming the rung and the policy that refused it.
func TestLadderRefusal_ModelNotAllowedNamesRungAndPolicy(t *testing.T) {
	d, p, _, hits, policyID := ladderDeps(t, ladderCase{models: []string{"openai/free"}}, time.Now())
	w := chat(d, p, "priced")
	wantAnswer(t, w, http.StatusForbidden, httpapi.APIErrorBody{
		Type: "invalid_request_error", Code: "model_not_allowed", Refusal: "model_not_allowed",
		Message: `model "priced" is not allowed by the lane policy ` + policyID,
	}, nil)
	if hits.Load() != 0 {
		t.Fatal("a refused model reached the upstream")
	}
}
