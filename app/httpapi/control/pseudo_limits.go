// POST /pseudo-limits puts a budget or a rate limit on an existing team, project or service account in one request (app/pseudo.ExpandLimits, through the policy step).

package control

import (
	"context"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/account"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/pkg/engine"
)

type pseudoLimitsInput struct {
	DryRun bool `query:"dryRun" doc:"Run every check, the plan check included, and answer what would be written, without writing it. With spec.budget and spec.rateLimit omitted, answer only case and current."`
	Body   pseudo.Limits
}

// PseudoLimitsAnswer is what POST /pseudo-limits wrote, or would write on a dry run.
type PseudoLimitsAnswer struct {
	Case           pseudo.PolicyCase `json:"case"                     enum:"none,own,copy" doc:"What the policy step did, as POST /pseudo-access answers it."`
	Current        *PolicyCurrent    `json:"current,omitempty"        doc:"With both limits omitted on a dry run: the policy the account is under now, absent for none. Nothing else is answered then."`
	Policy         *RowRef           `json:"policy,omitempty"         doc:"The policy the account is under once written, now linking the limit."`
	Budget         *PseudoLimitRef   `json:"budget,omitempty"         doc:"The budget made, with its link as stored."`
	RateLimit      *PseudoLimitRef   `json:"rateLimit,omitempty"      doc:"The rate limit made, with its link as stored."`
	UnpricedModels []string          `json:"unpricedModels,omitempty" doc:"With a budget: the models the target reaches now that have no pricing, as provider/model; requests for them answer unpriced_under_budget once the budget applies. A warning, not a refusal."`
	Created        []CreatedRow      `json:"created,omitempty"        doc:"Every row made, in write order."`
	Updated        []CreatedRow      `json:"updated,omitempty"        doc:"Every row updated, in write order."`
	Deleted        []CreatedRow      `json:"deleted,omitempty"        doc:"Every row deleted, in write order."`
	Warnings       []PseudoIssue     `json:"warnings"                 doc:"Plan warnings the rows written would raise that are not there now, on those rows (as the row carrying the warning, or the row it names). They refuse nothing; [] when none, and on the dry run without a limit. A row the caller may not see keeps its code and kind only."`
	Under          *PseudoUnder      `json:"under,omitempty"          doc:"For a team or project target: what the limit does to the lanes under it, which it applies to, such as a budget leaving a lane only unpriced models. Never a refusal; absent for a service account target and on the dry run without a limit."`
}

type pseudoLimitsOutput struct {
	Status int
	Body   PseudoLimitsAnswer
}

func registerPseudoLimits(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "create_pseudo_limits",
		Method:      http.MethodPost,
		Path:        "/pseudo-limits",
		Summary:     "Put a budget or rate limit on a team, project or service account",
		Description: "In one transaction, makes the budget or rate limit (exactly one of spec.budget and spec.rateLimit, each the spec its own create takes) owned by the target's scope, then links it from the policy over the target's account beside what that policy links already, by the cases POST /pseudo-access describes: none, own, copy, and 403 policy_set_above. A copy that would carry links the target's scope may not link answers 400 links_not_carried. " +
			"spec.link sets the link's own fields: shared (omitted means true) counts the limit once for everyone under the target, false once per account under it; models, with a rate limit only, scopes it to those models, checked as a policy update checks a rate-limit link's models (400 at /spec/link/models, as is models with a budget). The answer's budget or rateLimit carries the link as stored. " +
			"Each row passes what its own create, update or delete call passes, with the same answers; then the result is checked against the plans (422 plan_issue) and against what every other account reaches (422 other_accounts_changed). A budget's answer lists unpricedModels, the models the target uses now that the budget would refuse for want of pricing; it does not refuse. dryRun=true answers 200 with the same body and writes nothing; with both limits omitted it answers only case and current.",
		Tags:          []string{"policies"},
		Middlewares:   protect,
		DefaultStatus: http.StatusCreated,
		Errors:        []int{400, 401, 403, 404, 500},
		Responses:     pseudoResponses(api, "conflict: another change touched the same rows at the same time, retry; duplicate_binding from the binding. pointer names the row."),
	}, func(ctx context.Context, in *pseudoLimitsInput) (*pseudoLimitsOutput, error) {
		out := &pseudoLimitsOutput{Status: http.StatusOK}
		s := in.Body.Spec
		if s.Budget == nil && s.RateLimit == nil && in.DryRun {
			c, cur, err := d.policyPrefill(ctx, s.Target)
			out.Body = PseudoLimitsAnswer{Case: c, Current: cur, Warnings: []PseudoIssue{}}
			return out, err
		}
		var change pseudo.LimitsChange
		var target *account.Account
		var unpriced []string
		var under *PseudoUnder
		ctx = pseudo.AfterCheck(ctx, func(f plan.Findings) error {
			if err := checkPolicyStep(target.Meta.ID)(f); err != nil {
				return err
			}
			var err error
			if under, err = d.under(ctx, f, target); err != nil || s.Budget == nil {
				return err
			}
			base, err := f.Baseline()
			if err != nil {
				return err
			}
			unpriced = unpricedModels(base, f.Result, target.Meta.ID)
			return nil
		})
		expand := func(ctx context.Context, tx *appcatalog.Stores) ([]pseudo.Row, error) {
			if err := pseudo.CheckLimits(in.Body); err != nil {
				return nil, err
			}
			reads, err := d.boundTo(tx).policyReads(ctx, s.Target)
			if err != nil {
				return nil, err
			}
			target = reads.Account
			if reads.Case() != pseudo.CaseAbove {
				if err := d.checkLinkModels(&in.Body, reads.Account.Meta.Owner, reads.Account.Meta.Name); err != nil {
					return nil, err
				}
			}
			change, err = pseudo.ExpandLimits(in.Body, reads)
			return change.Rows, policyStepError(err)
		}
		w, rows, err := d.runPseudo(ctx, expand, in.DryRun)
		if err != nil {
			return nil, err
		}
		out.Body = PseudoLimitsAnswer{Case: change.Case, UnpricedModels: unpriced, Created: stepRows(rows, pseudo.OpCreate), Updated: stepRows(rows, pseudo.OpUpdate), Deleted: stepRows(rows, pseudo.OpDelete), Warnings: w.warnings, Under: under}
		if p := change.Policy; p != nil {
			ref := rowRef(&p.Meta)
			out.Body.Policy = &ref
		}
		if s.Budget != nil {
			out.Body.Budget = limitRef(change)
		} else {
			out.Body.RateLimit = limitRef(change)
		}
		if !in.DryRun {
			out.Status = http.StatusCreated
		}
		return out, nil
	})
	documentPlanIssue(api, "/pseudo-limits")
}

// unpricedModels lists, as provider/model, the models account accountID reaches in the baseline that in after it may not use for want of pricing under a budget.
func unpricedModels(baseline, after *engine.View, accountID string) []string {
	now := plan.ReachOf(baseline, accountID, nil)
	then := plan.ReachOf(after, accountID, nil)
	if now == nil || then == nil {
		return []string{}
	}
	reached := map[string]bool{}
	for _, m := range now.Models {
		reached[m.Model.Meta.ID] = true
	}
	out := []string{}
	for _, m := range then.Excluded {
		if m.Refused == nil || m.Refused.Code != plan.CodeUnpricedUnderBudget || !reached[m.Model.Meta.ID] {
			continue
		}
		ref := m.Model.Meta.Name
		if m.Provider != nil {
			ref = m.Provider.Meta.Name + "/" + ref
		}
		if !slices.Contains(out, ref) {
			out = append(out, ref)
		}
	}
	slices.Sort(out)
	return out
}
