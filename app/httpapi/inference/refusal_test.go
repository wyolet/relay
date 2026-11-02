package inference

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/plan"
)

func envelopeOf(t *testing.T, rec *httptest.ResponseRecorder) httpapi.APIErrorBody {
	t.Helper()
	var env httpapi.APIError
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body %q: %v", rec.Body, err)
	}
	return env.Err
}

// Every route refusal answers with its class beside the code, the codes relay renames on the wire included.
func TestMapRouteRefusal_CarriesTheRefusal(t *testing.T) {
	for _, tc := range []struct {
		code, wire string
		want       plan.RefusalKind
	}{
		{plan.CodeModelNotFound, plan.CodeModelNotFound, plan.RefusalHostNotReachable},
		{plan.CodeModelDisabled, plan.CodeModelDisabled, plan.RefusalHostNotReachable},
		{plan.CodeNoBinding, "no_host_binding", plan.RefusalHostNotReachable},
		{plan.CodeNoCredentials, "no_keys", plan.RefusalHostNotReachable},
		{plan.CodeModelNotAllowed, plan.CodeModelNotAllowed, plan.RefusalModelNotAllowed},
		{plan.CodePolicyDisabled, plan.CodePolicyDisabled, plan.RefusalModelNotAllowed},
		{plan.CodeUnpricedUnderBudget, plan.CodeUnpricedUnderBudget, plan.RefusalModelNotAllowed},
		{plan.CodeAccountClosed, plan.CodeAccountClosed, plan.RefusalModelNotAllowed},
	} {
		rec := httptest.NewRecorder()
		mapRouteRefusal(rec, &plan.Refusal{Code: tc.code, Message: "m"}, "gpt-4o")
		if got := envelopeOf(t, rec); got.Code != tc.wire || got.Refusal != string(tc.want) {
			t.Errorf("%s: code %q refusal %q, want %q %q", tc.code, got.Code, got.Refusal, tc.wire, tc.want)
		}
	}
}

// Refusals the pipeline makes before any upstream call are classed; an upstream's own failure is not a refusal.
func TestMapPipelineErr_CarriesTheRefusal(t *testing.T) {
	budget := limits.Counter{Kind: limits.KindBudget, Rung: limits.RungTeam, PolicyID: "p", ID: "b"}
	rule := limits.Counter{Kind: limits.KindRule, Rung: limits.RungLane, PolicyID: "p", ID: "r"}
	for name, tc := range map[string]struct {
		err    error
		status int
		code   string
		want   plan.RefusalKind
	}{
		"caller rate limit": {&pipeline.CallerLimitedError{Refused: &limits.Refused{Counter: &rule, RetryAfter: time.Second}}, http.StatusTooManyRequests, plan.CodeRateLimitExceeded, plan.RefusalNoCapacity},
		"caller budget":     {&pipeline.CallerLimitedError{Refused: &limits.Refused{Counter: &budget, RetryAfter: time.Hour}}, http.StatusTooManyRequests, plan.CodeBudgetExceeded, plan.RefusalNoCapacity},
		"upstream limits":   {&pipeline.UpstreamLimitedError{RetryAfter: time.Second}, http.StatusTooManyRequests, plan.CodeRateLimitExceeded, plan.RefusalNoCapacity},
		"every key broken":  {keypool.ErrNoHealthyKeys, http.StatusServiceUnavailable, plan.CodeNoHealthyKeys, plan.RefusalNoCapacity},
		"no key":            {pipeline.ErrNoKeys, http.StatusServiceUnavailable, "no_keys", plan.RefusalHostNotReachable},
		"upstream status":   {&pipeline.UpstreamFailureError{Status: 500, Cause: pipeline.ErrAllKeysExhausted}, http.StatusInternalServerError, "upstream_unavailable", ""},
		"unreachable":       {&pipeline.UpstreamUnreachableError{Host: "h", Attempts: 2, Cause: errors.New("dial")}, http.StatusBadGateway, "upstream_unreachable", ""},
		"transport":         {errors.New("reset"), http.StatusBadGateway, "upstream_error", ""},
	} {
		rec := httptest.NewRecorder()
		mapPipelineErr(rec, tc.err)
		got := envelopeOf(t, rec)
		if rec.Code != tc.status || got.Code != tc.code || got.Refusal != string(tc.want) {
			t.Errorf("%s: %d %q refusal %q, want %d %q %q", name, rec.Code, got.Code, got.Refusal, tc.status, tc.code, tc.want)
		}
		if refusal := pipeline.RefusalOf(tc.err); refusal != tc.want {
			t.Errorf("%s: RefusalOf = %q, want %q", name, refusal, tc.want)
		}
	}
}

// The usage event of a request relay refused carries the class beside error_kind: a route refusal, and a caller budget the pipeline refuses; a served request carries none.
func TestUsageEvent_CarriesTheRefusal(t *testing.T) {
	d, p, events, _, _ := ladderDeps(t, ladderCase{models: []string{"openai/free"}}, time.Now())
	for model, want := range map[string]struct {
		kind    string
		refusal plan.RefusalKind
	}{
		"priced":  {plan.CodeModelNotAllowed, plan.RefusalModelNotAllowed},
		"missing": {plan.CodeModelNotFound, plan.RefusalHostNotReachable},
		"free":    {"", ""},
	} {
		chat(d, p, model)
		if ev := events.next(t); ev.ErrorKind != want.kind || ev.Refusal != string(want.refusal) {
			t.Errorf("%s: event error_kind %q refusal %q, want %q %q", model, ev.ErrorKind, ev.Refusal, want.kind, want.refusal)
		}
	}

	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	d, p, events, _, _ = ladderDeps(t, ladderCase{budget: &budget.Spec{AmountUSD: "1", Period: budget.PeriodMonth, Mode: budget.ModeHard}}, now)
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		w := chat(d, p, "priced")
		ev := events.next(t)
		if w.Code == http.StatusOK && time.Now().Before(deadline) {
			continue
		}
		if ev.ErrorKind != "rate_limited" || ev.Refusal != string(plan.RefusalNoCapacity) {
			t.Fatalf("spent budget: %d, event error_kind %q refusal %q", w.Code, ev.ErrorKind, ev.Refusal)
		}
		break
	}
}

// A ladder refusal at the edge carries its class; a gate that is not a refusal carries none.
func TestWriteForbidden_CarriesTheRefusal(t *testing.T) {
	for code, want := range map[string]plan.RefusalKind{
		plan.CodeAccountNotHeld: plan.RefusalModelNotAllowed,
		plan.CodePolicyDisabled: plan.RefusalModelNotAllowed,
		"passthrough_forbidden": "",
	} {
		rec := httptest.NewRecorder()
		writeForbidden(rec, code, `no account "x" held`)
		got := envelopeOf(t, rec)
		if got.Code != code || got.Refusal != string(want) || got.Message != `no account "x" held` {
			t.Errorf("%s: %+v", code, got)
		}
	}
}
