package inference

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/usagelog"
)

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
		w := chat(d, p, model)
		ev := events.next(t)
		if ev.ErrorKind != want.kind || ev.Refusal != string(want.refusal) {
			t.Errorf("%s: event error_kind %q refusal %q, want %q %q", model, ev.ErrorKind, ev.Refusal, want.kind, want.refusal)
		}
		wantAnsweredRow(t, model, w, ev)
		if ev.LaneAccountID != p.Caller.Lane.Meta.ID {
			t.Errorf("%s: event lane_account_id %q, want the key's lane %s", model, ev.LaneAccountID, p.Caller.Lane.Meta.ID)
		}
		// A refused row names the model it refused: the catalog's when the name resolves, the requested string always.
		resolves := model != "missing"
		if ev.RequestedModel != model || (ev.ModelID != "") != resolves || (resolves && (ev.Model != model || ev.Provider != "openai")) {
			t.Errorf("%s: event requested_model %q model_id %q model %q provider %q", model, ev.RequestedModel, ev.ModelID, ev.Model, ev.Provider)
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
		if ev.ErrorKind != plan.CodeBudgetExceeded || ev.Refusal != string(plan.RefusalNoCapacity) {
			t.Fatalf("spent budget: %d, event error_kind %q refusal %q", w.Code, ev.ErrorKind, ev.Refusal)
		}
		wantAnsweredRow(t, "spent budget", w, ev)
		if strings.Contains(ev.ErrorMessage, "counter") || strings.Contains(ev.ErrorMessage, ":s:account:") {
			t.Errorf("spent budget: row message %q names the counter", ev.ErrorMessage)
		}
		break
	}
}

// wantAnsweredRow checks a row records what the caller was answered: its status and, for an error, the body's message.
func wantAnsweredRow(t *testing.T, name string, w *httptest.ResponseRecorder, ev usagelog.Event) {
	t.Helper()
	if ev.Status != w.Code {
		t.Errorf("%s: row status %d, caller got %d", name, ev.Status, w.Code)
	}
	if w.Code < 400 {
		return
	}
	if msg := envelopeOf(t, w).Message; ev.ErrorMessage != msg {
		t.Errorf("%s: row message %q, body message %q", name, ev.ErrorMessage, msg)
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
