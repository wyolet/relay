// Logs read-side endpoints: the full per-request lifecycle view that drives
// the frontend Logs page. A "log" is what happened on a request (routing,
// status, timing, tokens, errors, identity) — i.e. the full usage.Event —
// with the captured request/response bodies attached when payload logging
// was opted in for that request ("(not logged)" otherwise).
//
//	GET /logs                full lifecycle records, newest first, filterable
//	GET /logs/{request_id}   one record + its captured bodies (if any)
//
// Distinction from /usage: /usage is the narrow metrics projection (billing);
// /logs is the full record + optional body. Both read the same log event
// stream via usagelog.Reader; the body joins by request_id via payload.Reader.
package control

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/payloadlog"
	"github.com/wyolet/relay/app/usagelog"
)

// --- GET /logs ---

type logsListInput struct {
	usageFilterInput
	Limit  int    `query:"limit" doc:"Cap on returned records (page size). Default 100, max 10000."`
	Cursor string `query:"cursor" doc:"Opaque pagination cursor from a previous response's next_cursor. Returns the next (older) page."`
}

type logsListOutput struct {
	Body struct {
		Logs       []usagelog.Event `json:"logs"`
		NextCursor string           `json:"next_cursor,omitempty"`
	}
}

// --- GET /logs/{request_id} ---

type logGetInput struct {
	RequestID string `path:"request_id" doc:"The request id to fetch the full record + bodies for."`
}

// logPayload is the captured-body half of a log record, nil when payload
// logging wasn't opted in (or the capture aged out).
type logPayload struct {
	RequestBody       string `json:"request_body,omitempty"`
	ResponseBody      string `json:"response_body,omitempty"`
	RequestTruncated  bool   `json:"request_truncated,omitempty"`
	ResponseTruncated bool   `json:"response_truncated,omitempty"`
}

type logGetOutput struct {
	Body struct {
		Log usagelog.Event `json:"log"`
		// Payload carries the captured bodies; nil/absent means the request
		// ran without payload logging (or the body has aged out).
		Payload *logPayload `json:"payload,omitempty"`
	}
}

func registerLogs(api huma.API, d Deps, protect huma.Middlewares) {
	if d.UsageReader == nil {
		return
	}

	huma.Register(api, huma.Operation{
		OperationID: "logs_list",
		Method:      http.MethodGet,
		Path:        "/logs",
		Summary:     "List full per-request log records (newest first), filterable",
		Description: "The full lifecycle view: routing, status, timing, tokens, " +
			"errors, identity — one record per request. Filter by the same " +
			"dimensions as /usage/events. Captured bodies are attached only on " +
			"the detail endpoint (GET /logs/{request_id}), not in the list.",
		Tags:        []string{"logs"},
		Middlewares: protect,
		Errors:      []int{400, 401, 500},
	}, func(ctx context.Context, in *logsListInput) (*logsListOutput, error) {
		if err := d.Authz.Authorize(ctx, "logs.list", authz.Resource{Kind: "logs"}); err != nil {
			return nil, mapAuthzErr(err)
		}
		q, err := in.toEventQuery()
		if err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		q.Limit = in.Limit
		if in.Cursor != "" {
			ts, id, err := decodeCursor(in.Cursor)
			if err != nil {
				return nil, err
			}
			q.CursorTS, q.CursorID = ts, id
		}
		events, err := d.UsageReader.Events(ctx, q)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if events == nil {
			events = []usagelog.Event{}
		}
		out := &logsListOutput{}
		out.Body.Logs = events
		if n := len(events); n > 0 && n == effectiveLimit(in.Limit) {
			last := events[n-1]
			out.Body.NextCursor = encodeCursor(last.Timestamp, last.RequestID)
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "logs_get",
		Method:      http.MethodGet,
		Path:        "/logs/{request_id}",
		Summary:     "Fetch one request's full record + captured bodies",
		Description: "Returns the full log record for a single request id, with " +
			"the captured request/response bodies attached when payload logging " +
			"was opted in (else payload is null). 404 when no log record exists " +
			"for the id.",
		Tags:        []string{"logs"},
		Middlewares: protect,
		Errors:      []int{401, 404, 500},
	}, func(ctx context.Context, in *logGetInput) (*logGetOutput, error) {
		if err := d.Authz.Authorize(ctx, "logs.get", authz.Resource{Kind: "logs"}); err != nil {
			return nil, mapAuthzErr(err)
		}
		events, err := d.UsageReader.Events(ctx, usagelog.EventQuery{RequestID: in.RequestID, Limit: 1})
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if len(events) == 0 {
			return nil, huma.Error404NotFound("no log record for request id " + in.RequestID)
		}
		out := &logGetOutput{}
		out.Body.Log = events[0]
		// Attach the captured body if payload logging is wired and a capture
		// exists. Absence is normal (opt-in), not an error.
		if d.PayloadReader != nil {
			rec, perr := d.PayloadReader.Get(ctx, in.RequestID)
			switch {
			case perr == nil:
				out.Body.Payload = &logPayload{
					RequestBody:       string(rec.RequestBody),
					ResponseBody:      string(rec.ResponseBody),
					RequestTruncated:  rec.RequestTruncated,
					ResponseTruncated: rec.ResponseTruncated,
				}
			case errors.Is(perr, payloadlog.ErrNotFound):
				// no capture for this request — leave Payload nil
			default:
				return nil, huma.Error500InternalServerError(perr.Error())
			}
		}
		return out, nil
	})
}
