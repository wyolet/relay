// Checks the pseudo kinds run on the plans their rows make, beside the plan check: the target's lane is not left refusing every request, and no account but the target and those under it reaches anything else than it does now. The accounts a write changes are found by comparing the result with the baseline (plan.ChangedReach), whatever row changed them: a policy or pool edited, a pool deleted, a share revoked.

package control

import (
	"context"
	"fmt"
	"net/http"

	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/pkg/engine"
)

// codeOthersChanged refuses rows that would change what an account other than the target may use.
const codeOthersChanged = "other_accounts_changed"

// checkPolicyStep returns the checks of what the plan check found that it does not make itself: the target's lane is not refused as a whole where it was not, also when its policy changed in place with no binding written (laneServed), and no other account's reach changed (othersUnchanged). Both compare the result with the baseline built from the same rows, never with the live view, which may lag. No rows written, nothing is compared.
func checkPolicyStep(targetID string, rows []pseudo.Row) func(plan.Findings) error {
	return func(f plan.Findings) error {
		if len(rows) == 0 {
			return nil
		}
		if err := laneServed(f, targetID); err != nil {
			return err
		}
		return othersUnchanged(targetID)(f)
	}
}

// laneServed refuses a result in which the lane of account id (a team's or project's own account read as a lane directly under it) refuses every request, unless it does with the same code in the baseline.
func laneServed(f plan.Findings, id string) error {
	refused := func(v *engine.View) *plan.Refusal {
		if r := plan.ReachOf(v, id, nil); r != nil {
			return r.Refused
		}
		return nil
	}
	now := refused(f.Result)
	if now == nil {
		return nil
	}
	base, err := f.Baseline()
	if err != nil {
		return err
	}
	if prev := refused(base); prev != nil && prev.Code == now.Code {
		return nil
	}
	r := refusal(&httpapi.APIError{
		Err:        httpapi.APIErrorBody{Type: "invalid_request_error", Code: codePlanIssue, Message: "the rows pass one by one but would leave the target's account refusing every request; nothing was written"},
		HTTPStatus: http.StatusUnprocessableEntity,
	}, "")
	r.Issues = []PseudoIssue{{Code: now.Code, Message: "requests in this account would all be refused", Kind: string(plan.KindAccount), ID: id, Pointer: "/spec/target"}}
	return r
}

// othersUnchanged returns the check of what the plan check found: every account but targetID's and those whose ladder climbs it (the projects and lanes under a team, the lanes under a project) reaches in the result what it reaches in the baseline, no less and no more. Every account counts, those the caller may not see included; the refusal counts them and names none.
func othersUnchanged(targetID string) func(plan.Findings) error {
	return func(f plan.Findings) error {
		base, err := f.Baseline()
		if err != nil {
			return err
		}
		var changed []plan.ReachChange
		if err := f.Within(func(ctx context.Context) error {
			changed, err = plan.ChangedReach(ctx, base, f.Result, inSubtree(targetID))
			return err
		}); err != nil {
			return err
		}
		if len(changed) == 0 {
			return nil
		}
		narrowed := 0
		for _, c := range changed {
			if c.Narrowed {
				narrowed++
			}
		}
		return refusal(&httpapi.APIError{
			Err:        httpapi.APIErrorBody{Type: "invalid_request_error", Code: codeOthersChanged, Message: fmt.Sprintf("the change would alter what %d other accounts may use (%d of them would lose something); nothing was written", len(changed), narrowed)},
			HTTPStatus: http.StatusUnprocessableEntity,
		}, "")
	}
}
