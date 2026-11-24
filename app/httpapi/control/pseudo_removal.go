// POST /pseudo-agent-removals deletes a service account and what was made for it in one request (app/pseudo.ExpandAgentRemoval).

package control

import (
	"context"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/pseudo"
)

type pseudoAgentRemovalInput struct {
	DryRun bool `query:"dryRun" doc:"Run every check, the plan check included, and answer what would be deleted and kept, without deleting it."`
	Body   pseudo.AgentRemoval
}

// PseudoAgentRemovalAnswer is what POST /pseudo-agent-removals deleted, or would delete on a dry run.
type PseudoAgentRemovalAnswer struct {
	Deleted  []CreatedRow  `json:"deleted"  doc:"Every row deleted, in delete order: the service account's keys, the binding that held only its account, the shares it held and bindings their revoke left empty, the service account, its account, then the labelled policies and pools that go."`
	Kept     []PseudoKept  `json:"kept"     doc:"The policies and pools labelled agent-id=<service account id> that stay, with why. A labelled row owned outside the service account's scope that the caller may not see is not listed."`
	Updated  []CreatedRow  `json:"updated"  doc:"Every row changed and kept, in write order: the binding the service account's account was taken out of when it holds other accounts too, and the bindings, keys and pools a share's revoke changed."`
	Warnings []PseudoIssue `json:"warnings" doc:"Plan warnings the deletes would raise that are not there now, on the rows deleted or changed (as the row carrying the warning, or the row it names). They refuse nothing; [] when none. A row the caller may not see keeps its code and kind only."`
}

// PseudoKept is a labelled policy or pool that stays.
type PseudoKept struct {
	Kind   string            `json:"kind"           enum:"policy,pool" doc:"policy or pool."`
	ID     string            `json:"id"             doc:"Id of the row."`
	Name   string            `json:"name,omitempty" doc:"Slug of the row; omitted when the caller may not see it."`
	Reason pseudo.KeptReason `json:"reason"         enum:"in_use,kept_by_request,not_visible,not_deletable" doc:"in_use: a binding or policy the request does not delete still uses it, seen by the caller or not. kept_by_request: spec.keep names it. not_visible: the service account's scope owns it but the caller may not see it. not_deletable: it is owned outside the service account's scope, or its delete's grant or governance refuses the caller."`
}

// codeChanged refuses a removal whose set to delete is no longer the one spec.expect names.
const codeChanged = "changed"

// removalChanged is the 409 changed answer: the refusal and the set as it is now.
type removalChanged struct {
	httpapi.APIError
	Pointer string       `json:"pointer,omitempty" doc:"/spec/expect."`
	Deleted []CreatedRow `json:"deleted"           doc:"What the request would delete now, as a dry run answers it."`
	Kept    []PseudoKept `json:"kept"              doc:"What it would keep now, as a dry run answers it."`
}

type pseudoAgentRemovalOutput struct {
	Body PseudoAgentRemovalAnswer
}

func registerPseudoAgentRemoval(api huma.API, d Deps, protect huma.Middlewares) {
	responses := pseudoResponses(api, "")
	responses["409"] = jsonResponse(api, "changed: spec.expect is no longer exactly the set the request would delete; deleted and kept say what it is now, and nothing was deleted. resource_in_use, with the row's blockers in the message: a row still references one the request deletes, such as a role binding naming the service account. managed_by_source: an identity source manages the service account. conflict: another change touched the same rows at the same time, retry. pointer names the part of the body.", removalChanged{})
	huma.Register(api, huma.Operation{
		OperationID: "create_pseudo_agent_removal",
		Method:      http.MethodPost,
		Path:        "/pseudo-agent-removals",
		Summary:     "Delete a service account and what was made for it",
		Description: "In one transaction, deletes the service account's keys, takes its account out of the policy binding holding it as PUT /accounts/{ref}/policy with a null policyId does (the binding is deleted when it held only that account, else shrinks), revokes the shares the service account holds as DELETE /accounts/by-id/{id} does, deletes the service account, which takes its account along, then the policies and pools labelled agent-id=<service account id> that its scope owns, the caller may see and delete, spec.keep does not name, and no binding or policy left after the earlier deletes uses, those the caller may not see included. The other labelled rows stay and are listed in kept with why; one owned outside the scope that the caller may not see is neither touched nor listed. " +
			"Each delete passes the grant, governance and reference check of its own delete call, with the same answers, pointer /spec/serviceAccountId; a refused delete refuses the request and nothing is deleted. Then the result is checked against the plans (422 plan_issue: an error on a row that stays that is not there now) and against what every other account reaches (422 other_accounts_changed). A deleted key stops authenticating once this pod's plans hear the delete, as after DELETE /keys/by-id/{id}. " +
			"dryRun=true does everything but the commit and answers 200 with the same body. spec.expect, given, must be exactly the ids of the deleted list: when the set differs, the request answers 409 changed with the current deleted and kept lists and deletes nothing. Each row deleted or changed is audited as its own delete call records it.",
		Tags:        []string{"service-accounts"},
		Middlewares: protect,
		Errors:      []int{400, 401, 403, 404, 500},
		Responses:   responses,
	}, func(ctx context.Context, in *pseudoAgentRemovalInput) (*pseudoAgentRemovalOutput, error) {
		var change pseudo.RemovalChange
		laneID := ""
		ctx = pseudo.AfterCheck(ctx, func(f plan.Findings) error {
			return othersUnchanged(laneID)(f)
		})
		expand := func(ctx context.Context, tx *appcatalog.Stores) ([]pseudo.Row, error) {
			if err := pseudo.CheckAgentRemoval(in.Body); err != nil {
				return nil, err
			}
			reads, err := d.boundTo(tx).removalReads(ctx, in.Body)
			if err != nil {
				return nil, err
			}
			if reads.Lane != nil {
				laneID = reads.Lane.Meta.ID
			}
			if change, err = pseudo.ExpandAgentRemoval(in.Body, reads); err != nil {
				return nil, err
			}
			if expect := in.Body.Spec.Expect; expect != nil && !sameIDs(expect, stepRows(change.Rows, pseudo.OpDelete)) {
				return nil, &removalChanged{
					APIError: httpapi.APIError{Err: httpapi.APIErrorBody{Type: "invalid_request_error", Code: codeChanged, Message: "the rows this request would delete are no longer the ones spec.expect names; nothing was deleted. deleted and kept say what the request would do now"}, HTTPStatus: http.StatusConflict},
					Pointer:  "/spec/expect", Deleted: stepRows(change.Rows, pseudo.OpDelete), Kept: keptRows(change.Kept),
				}
			}
			return change.Rows, nil
		}
		w, rows, err := d.runPseudo(ctx, expand, in.DryRun)
		if err != nil {
			return nil, err
		}
		return &pseudoAgentRemovalOutput{Body: PseudoAgentRemovalAnswer{
			Deleted: stepRows(rows, pseudo.OpDelete), Kept: keptRows(change.Kept), Updated: stepRows(rows, pseudo.OpUpdate), Warnings: w.warnings,
		}}, nil
	})
	documentPlanIssue(api, "/pseudo-agent-removals")
}

// keptRows answers the kept rows, without the name of one the caller may not see.
func keptRows(kept []pseudo.KeptRow) []PseudoKept {
	out := []PseudoKept{}
	for _, k := range kept {
		row := PseudoKept{Kind: k.Kind, ID: k.Meta.ID, Reason: k.Reason}
		if k.Visible {
			row.Name = k.Meta.Name
		}
		out = append(out, row)
	}
	return out
}

// sameIDs reports whether expect holds exactly the ids of rows, in any order.
func sameIDs(expect []string, rows []CreatedRow) bool {
	got := make([]string, len(rows))
	for i, r := range rows {
		got[i] = r.ID
	}
	want := slices.Clone(expect)
	slices.Sort(got)
	slices.Sort(want)
	return slices.Equal(want, got)
}
