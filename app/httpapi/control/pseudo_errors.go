// How a refused pseudo object is answered: the refusal envelope with the pointer of the part of the body at fault, and the plan issues projected for the caller.

package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/meta"
)

// pseudoError answers a pseudo write that wrote nothing.
func (d Deps) pseudoError(ctx context.Context, err error) error {
	var be *pseudo.BodyError
	var ie *pseudo.IssuesError
	var re *pseudo.RowError
	var answered *pseudoRefusal
	var ue *pseudo.UndoError
	var oe *pseudo.OrganizationError
	var changed *removalChanged
	switch {
	case errors.As(err, &answered):
		return answered
	case errors.As(err, &changed):
		return changed
	case errors.As(err, &ue):
		slog.ErrorContext(ctx, "control: pseudo write failed and left behind what it wrote before its transaction", "left", ue.Left, "err", ue)
		return refusal(huma.Error500InternalServerError(fmt.Sprintf("nothing was written, but removing what was written before the transaction failed: %s is left behind", strings.Join(ue.Left, ", "))), "")
	case errors.As(err, &oe):
		return organizationRefusal(oe)
	case errors.As(err, &be):
		return refusal(huma.Error400BadRequest(be.Message), be.Pointer)
	case errors.As(err, &ie):
		r := refusal(&httpapi.APIError{
			Err:        httpapi.APIErrorBody{Type: "invalid_request_error", Code: codePlanIssue, Message: fmt.Sprintf("the rows pass one by one but together would raise errors in the plans (%d, listed in issues); nothing was written", len(ie.Issues))},
			HTTPStatus: http.StatusUnprocessableEntity,
		}, "")
		r.Issues = d.pseudoIssues(ctx, ie.Rows, ie.Issues)
		return r
	case errors.Is(err, plan.ErrCheckTimeout):
		return refusal(huma.Error503ServiceUnavailable("checking the rows against the plans took too long; nothing was written, retry the request"), "")
	case errors.Is(err, pseudo.ErrNoPlans):
		return refusal(huma.Error503ServiceUnavailable("the plans are not loaded yet; nothing was written, retry the request"), "")
	}
	pointer := ""
	if errors.As(err, &re) {
		pointer, err = re.Row.Pointer, re.Err
	}
	if storage.IsConflict(err) || errors.Is(err, meta.ErrStaleResourceVersion) {
		return refusal(conflictError("another change touched the same rows at the same time; nothing was written, retry the request"), pointer)
	}
	if storage.IsDuplicate(err) || errors.Is(err, directory.ErrConflict) {
		return refusal(conflictError("another row holds a name or id this row takes: the given metadata.name, or one another change took at the same time; nothing was written"), pointer)
	}
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		return refusal(&httpapi.APIError{
			Err:        httpapi.APIErrorBody{Type: "invalid_request_error", Code: codeClientCanceled, Message: "the request ended before the write did; nothing was written"},
			HTTPStatus: statusClientClosed,
		}, pointer)
	}
	var se huma.StatusError
	if !errors.As(err, &se) && re != nil {
		err = mapWriteErr(re.Row.Kind, re.Row.Meta.ID, err)
	}
	if errors.As(err, &se) && se.GetStatus() < http.StatusInternalServerError {
		return refusal(err, pointer)
	}
	slog.ErrorContext(ctx, "control: pseudo write failed; nothing was written", "pointer", pointer, "err", err)
	return refusal(huma.Error500InternalServerError("the write failed on the server; nothing was written"), pointer)
}

// A request whose client went away before the write ended: no one reads the answer, but the audit row and the access log do.
const (
	codeClientCanceled = "client_canceled"
	statusClientClosed = 499
)

// refusal puts err in the pseudo refusal envelope with pointer.
func refusal(err error, pointer string) *pseudoRefusal {
	var ae *httpapi.APIError
	if inUse, ok := errors.AsType[*resourceInUseError](err); ok {
		return &pseudoRefusal{APIError: inUse.APIError, Pointer: pointer, Blockers: inUse.Blockers}
	}
	if !errors.As(err, &ae) {
		var se huma.StatusError
		status := http.StatusInternalServerError
		if errors.As(err, &se) {
			status = se.GetStatus()
		}
		ae = &httpapi.APIError{Err: httpapi.APIErrorBody{Type: "invalid_request_error", Message: err.Error()}, HTTPStatus: status}
	}
	return &pseudoRefusal{APIError: *ae, Pointer: pointer}
}

// pseudoIssues projects the plan check's issues on rows for the caller: an issue on one of the new rows (or a plan object of the same id) carries the row's pointer; one on a row the caller may not see keeps its code and kind only.
func (d Deps) pseudoIssues(ctx context.Context, rows []pseudo.Row, issues []engine.Issue) []PseudoIssue {
	ours := make(map[string]pseudo.Row, len(rows))
	for _, r := range rows {
		ours[r.Meta.ID] = r
	}
	var live *engine.View
	if d.PlanView != nil {
		live = d.PlanView()
	}
	out := make([]PseudoIssue, 0, len(issues))
	for _, i := range issues {
		pi := PseudoIssue{Code: i.Code, Kind: string(i.Source.Kind)}
		if r, ok := ours[i.Source.ID]; ok {
			pi.ID, pi.Name, pi.Field, pi.Message, pi.Pointer = r.Meta.ID, r.Meta.Name, i.Field, i.Message, r.Pointer
		} else if live != nil && issueVisible(ctx, d.Authz, plan.KindSingular(i.Source.Kind), i.Source.ID, plan.OwnerOf(live, i.Source)) {
			pi.ID, pi.Name, pi.Field, pi.Message = i.Source.ID, plan.NameOf(live, i.Source), i.Field, i.Message
		} else {
			pi.Message = "a row you may not see would break"
		}
		out = append(out, pi)
	}
	return out
}
