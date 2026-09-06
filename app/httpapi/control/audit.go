// Audit read endpoint: the admin-plane history of who changed what.
//
//	GET /audit   audit events, newest first, filterable + keyset-paginated
//
// Distinct from /logs: /logs is the data plane's per-request record,
// /audit is the control plane's. Only paths are recorded for a change,
// never values.
package control

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
)

type auditListInput struct {
	ActorID      string   `query:"actor_id" doc:"Match the acting user's id."`
	ActorName    string   `query:"actor_name" doc:"Match the acting user's name."`
	Action       []string `query:"action" doc:"Match any of the given action strings (e.g. \"policies.update\")."`
	ResourceKind []string `query:"resource_kind" doc:"Match any of the given resource kinds (API singular, e.g. \"policy\")."`
	ResourceID   string   `query:"resource_id" doc:"Match a single resource id."`
	Scope        []string `query:"scope" doc:"Match any of the given scopes, as \"project:<id>\" or \"team:<id>\"."`
	Status       string   `query:"status" enum:"allowed,denied,error" doc:"Match one outcome status."`
	From         string   `query:"from" doc:"Absolute lower bound (RFC3339)."`
	To           string   `query:"to" doc:"Absolute upper bound (RFC3339)."`
	Limit        int      `query:"limit" doc:"Cap on returned rows (page size). Default 100, max 10000."`
	Cursor       string   `query:"cursor" doc:"Opaque pagination cursor from a previous response's next_cursor. Returns the next (older) page."`
}

type auditListOutput struct {
	Body struct {
		Events     []audit.Event `json:"events"`
		NextCursor string        `json:"next_cursor,omitempty"`
	}
}

func registerAudit(api huma.API, d Deps, protect huma.Middlewares) {
	if d.AuditReader == nil {
		return
	}

	huma.Register(api, huma.Operation{
		OperationID: "audit_list",
		Method:      http.MethodGet,
		Path:        "/audit",
		Summary:     "List admin audit events (newest first), filterable",
		Description: "Every mutating control-plane action, every denial, and " +
			"every login/logout — one row per request. A change lists the JSON " +
			"paths it touched; values are never recorded.",
		Tags:        []string{"audit"},
		Middlewares: protect,
		Errors:      []int{400, 401, 403, 500},
	}, func(ctx context.Context, in *auditListInput) (*auditListOutput, error) {
		if err := d.Authz.Authorize(ctx, "audit.read", authz.Resource{Kind: "audit"}); err != nil {
			return nil, mapAuthzErr(err)
		}
		from, err := parseTime("from", in.From)
		if err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		to, err := parseTime("to", in.To)
		if err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		q := audit.Query{
			ActorID:       in.ActorID,
			ActorName:     in.ActorName,
			Actions:       in.Action,
			ResourceKinds: in.ResourceKind,
			ResourceID:    in.ResourceID,
			Scopes:        in.Scope,
			Status:        in.Status,
			From:          from,
			To:            to,
			Limit:         in.Limit,
		}
		if in.Cursor != "" {
			ts, id, err := decodeCursor(in.Cursor)
			if err != nil {
				return nil, err
			}
			q.CursorTS, q.CursorID = ts, id
		}
		events, err := d.AuditReader.Events(ctx, q)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if events == nil {
			events = []audit.Event{}
		}
		out := &auditListOutput{}
		out.Body.Events = events
		if n := len(events); n > 0 && n == audit.EffectiveLimit(in.Limit) {
			last := events[n-1]
			out.Body.NextCursor = encodeCursor(last.TS, last.ID)
		}
		return out, nil
	})
}
