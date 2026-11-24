// Checks the access and limits kinds run on the plans their rows make, beside the plan check: the target's lane is not left refusing every request, and no account but the target may reach other models or hosts than it does now. The rows are built so they cannot (a shared policy is copied, not edited; only a labelled pool nothing else links is changed or deleted); this refuses the write if they ever would.

package control

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/engine"
)

// codeOthersChanged refuses rows that would change what an account other than the target may use.
const codeOthersChanged = "other_accounts_changed"

// checkPolicyStep returns the checks of what the plan check found for rows that it does not make itself: the target's lane is not refused as a whole where it was not, also when its policy changed in place with no binding written (laneServed), and no other account's reach changed (othersUnchanged). Both compare the result with the baseline built from the same rows, never with the live view, which may lag.
func checkPolicyStep(rows []pseudo.Row, targetID string) func(plan.Findings) error {
	return func(f plan.Findings) error {
		if err := laneServed(f, targetID); err != nil {
			return err
		}
		return othersUnchanged(rows, targetID)(f)
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

// othersUnchanged returns the check of what the plan check found for rows: every account but targetID bound, in the result or the baseline, to a policy the rows update or to one linking a pool they update or delete reaches in the result what it reaches in the baseline. Every such account counts, those the caller may not see included; the refusal counts them and names none.
func othersUnchanged(rows []pseudo.Row, targetID string) func(plan.Findings) error {
	return func(f plan.Findings) error {
		policies, pools := map[string]bool{}, map[string]bool{}
		for _, r := range rows {
			switch {
			case r.Kind == "policy" && r.Op == pseudo.OpUpdate:
				policies[r.Meta.ID] = true
			case r.Kind == "pool" && r.Op != pseudo.OpCreate:
				pools[r.Meta.ID] = true
			}
		}
		if len(policies) == 0 && len(pools) == 0 {
			return nil
		}
		accounts := boundThrough(f.Result, policies, pools)
		if len(accounts) == 0 || slices.Equal(accounts, []string{targetID}) {
			return nil
		}
		base, err := f.Baseline()
		if err != nil {
			return err
		}
		for _, id := range boundThrough(base, policies, pools) {
			if !slices.Contains(accounts, id) {
				accounts = append(accounts, id)
			}
		}
		changed := 0
		for _, id := range accounts {
			if id != targetID && reachKey(plan.ReachOf(base, id, nil)) != reachKey(plan.ReachOf(f.Result, id, nil)) {
				changed++
			}
		}
		if changed == 0 {
			return nil
		}
		return refusal(&httpapi.APIError{
			Err:        httpapi.APIErrorBody{Type: "invalid_request_error", Code: codeOthersChanged, Message: fmt.Sprintf("the change would alter what %d other accounts may use; nothing was written", changed)},
			HTTPStatus: http.StatusUnprocessableEntity,
		}, "")
	}
}

// boundThrough lists, once each, the accounts the enabled bindings in v bind to a policy in policies or to one linking a pool in pools.
func boundThrough(v *engine.View, policies, pools map[string]bool) []string {
	var out []string
	for b := range engine.All[policybinding.PolicyBinding](v) {
		if !b.IsEnabled() {
			continue
		}
		hit := policies[b.Spec.PolicyID]
		if p := engine.Get[policy.Policy](v, b.Spec.PolicyID); !hit && p != nil {
			hit = slices.ContainsFunc(p.Spec.PoolIDs, func(id string) bool { return pools[id] })
		}
		if !hit {
			continue
		}
		for _, id := range b.Spec.AccountIDs {
			if !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
	}
	return out
}

// reachKey is what a lane reaches, as one comparable string: each model with its hosts and the pools it comes through, and the refusal of the whole lane.
func reachKey(r *plan.Reach) string {
	if r == nil {
		return ""
	}
	var parts []string
	if r.Refused != nil {
		parts = append(parts, "refused:"+r.Refused.Code)
	}
	for _, m := range r.Models {
		var hosts, via []string
		for _, h := range m.Hosts {
			hosts = append(hosts, h.Meta.ID)
		}
		for _, v := range m.Via {
			if v.Pool != nil {
				via = append(via, v.Pool.Meta.ID)
			}
			if v.Account != nil {
				via = append(via, v.Account.Meta.ID)
			}
		}
		parts = append(parts, m.Model.Meta.ID+"@"+strings.Join(hosts, ",")+"/"+strings.Join(via, ","))
	}
	slices.Sort(parts)
	return strings.Join(parts, ";")
}
