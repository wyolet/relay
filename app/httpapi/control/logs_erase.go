// Captured-body erasure: deletes the request/response bodies payload logging
// stored for a project and/or principal. Log (usage) events are left alone;
// they hold numbers and ids, not content.
//
//	POST /logs/erase   {"projectId": "...", "principalId": "..."}

package control

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/payloadlog"
)

type logsEraseInput struct {
	Body struct {
		ProjectID   string `json:"projectId,omitempty" doc:"Erase bodies captured for this project id."`
		PrincipalID string `json:"principalId,omitempty" doc:"Erase bodies captured for this principal (user or service account) id. With projectId, only that principal's bodies in that project."`
	}
}

type logsEraseOutput struct {
	Body struct {
		Requests uint64 `json:"requests" doc:"Captured requests matched and erased."`
	}
}

func registerLogsErase(api huma.API, d Deps, protect huma.Middlewares) {
	if d.PayloadReader == nil {
		return
	}
	huma.Register(api, huma.Operation{
		OperationID: "logs_erase",
		Method:      http.MethodPost,
		Path:        "/logs/erase",
		Summary:     "Erase captured bodies by project and/or principal",
		Description: "Deletes the captured request/response bodies of a project, " +
			"a principal, or one principal within a project, and returns once the " +
			"backend has deleted them. Log records (usage, cost, timing) are kept. " +
			"Admin only. 501 when the payload backend cannot erase.",
		Tags:        []string{"logs"},
		Middlewares: protect,
		Errors:      []int{400, 401, 403, 500, 501},
	}, func(ctx context.Context, in *logsEraseInput) (*logsEraseOutput, error) {
		f := payloadlog.EraseFilter{ProjectID: in.Body.ProjectID, PrincipalID: in.Body.PrincipalID}
		res := eraseResource(f)
		if err := d.Authz.Authorize(ctx, "logs.erase", authz.Resource{Kind: res.Kind, ID: res.ID, Owner: res.Owner}); err != nil {
			return nil, mapAuthzErr(err)
		}
		// Under single mode every signed-in user passes Authorize; erasing
		// another tenant's captures stays an operator action.
		if !authz.IsAdmin(ctx) {
			audit.Record(ctx, "logs.erase", res, audit.StatusDenied)
			return nil, huma.Error403Forbidden("captured bodies may only be erased by an admin")
		}
		if f.ProjectID == "" && f.PrincipalID == "" {
			return nil, huma.Error400BadRequest("projectId or principalId is required")
		}
		eraser, ok := d.PayloadReader.(payloadlog.Eraser)
		if !ok {
			return nil, huma.Error501NotImplemented("the payload backend cannot erase captured bodies")
		}
		erased, err := eraser.Erase(ctx, f)
		switch {
		case errors.Is(err, payloadlog.ErrEraseUnsupported):
			return nil, huma.Error501NotImplemented(err.Error())
		case err != nil:
			// Some bodies may already be gone, so the attempt is recorded.
			audit.Record(ctx, "logs.erase", res, audit.StatusError)
			return nil, huma.Error500InternalServerError(err.Error())
		}
		audit.Record(ctx, "logs.erase", res, audit.StatusAllowed)
		out := &logsEraseOutput{}
		out.Body.Requests = erased.Requests
		return out, nil
	})
}

// eraseResource carries the filter into the audit row: the project as the
// owner, the principal as the id.
func eraseResource(f payloadlog.EraseFilter) audit.Resource {
	res := audit.Resource{Kind: "logs", ID: f.PrincipalID}
	if f.ProjectID != "" {
		res.Owner = &meta.Owner{Kind: meta.OwnerProject, ID: f.ProjectID}
	}
	return res
}
