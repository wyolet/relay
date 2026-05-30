// Payload read-side endpoints: surface the request/response bodies the
// payload-logging observer captures for opted-in requests. Backed by a
// payloadlog.Reader, so the store can swap (file today, object store later)
// without touching this layer.
//
//	GET /payloads                raw capture metadata, newest first, filterable
//	GET /payloads/{request_id}   full request + response bodies for one request
//
// The split is deliberate: List never ships bodies (the Logs table reads it
// per page); Get returns the bodies for the detail view.
package control

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/payloadlog"
)

// --- shared input filter ---

type payloadFilterInput struct {
	Since        string   `query:"since" doc:"Relative window (e.g. \"1h\", \"24h\", \"7d\"). Default \"1h\". Ignored when from is set."`
	From         string   `query:"from" doc:"Absolute lower bound (RFC3339). Overrides since."`
	To           string   `query:"to" doc:"Absolute upper bound (RFC3339)."`
	RelayKeyHash []string `query:"relay_key_hash" doc:"Match any of the given sha256 hashes of the inbound bearer."`
	PolicyID     []string `query:"policy_id" doc:"Match any of the given Policy.metadata.id values."`
	ModelID      []string `query:"model_id" doc:"Match any of the given Model.metadata.id values."`
	HostID       []string `query:"host_id" doc:"Match any of the given Host.metadata.id values."`
	Source       []string `query:"source" doc:"Match any of \"pipeline\" | \"proxy\" | \"ws\" | \"batch\"."`
	ErrorKind    []string `query:"error_kind" doc:"Match any of the given error_kind values."`
	StatusMin    int      `query:"status_min" doc:"Minimum HTTP status to include."`
	StatusMax    int      `query:"status_max" doc:"Maximum HTTP status to include."`
}

func (f payloadFilterInput) toQuery() (payloadlog.Query, error) {
	since, err := parseSince(f.Since)
	if err != nil {
		return payloadlog.Query{}, err
	}
	from, err := parseTime("from", f.From)
	if err != nil {
		return payloadlog.Query{}, err
	}
	to, err := parseTime("to", f.To)
	if err != nil {
		return payloadlog.Query{}, err
	}
	if !from.IsZero() && !to.IsZero() && to.Before(from) {
		return payloadlog.Query{}, huma.Error400BadRequest("`to` must not be before `from`")
	}
	return payloadlog.Query{
		Since:        since,
		From:         from,
		To:           to,
		RelayKeyHash: f.RelayKeyHash,
		PolicyID:     f.PolicyID,
		ModelID:      f.ModelID,
		HostID:       f.HostID,
		Source:       f.Source,
		ErrorKind:    f.ErrorKind,
		StatusMin:    f.StatusMin,
		StatusMax:    f.StatusMax,
	}, nil
}

// --- GET /payloads ---

type payloadListInput struct {
	payloadFilterInput
	Limit  int    `query:"limit" doc:"Cap on returned records (page size). Default 100, max 10000."`
	Cursor string `query:"cursor" doc:"Opaque pagination cursor from a previous response's next_cursor. Returns the next (older) page."`
}

type payloadListOutput struct {
	Body struct {
		// Records carry metadata only — bodies are stripped. Fetch bodies
		// via GET /payloads/{request_id}.
		Records []payloadlog.Record `json:"records"`
		// NextCursor is set when a full page was returned (more may exist);
		// pass it back as ?cursor= for the next page. Empty on the last page.
		NextCursor string `json:"next_cursor,omitempty"`
	}
}

// --- GET /payloads/{request_id} ---

type payloadGetInput struct {
	RequestID string `path:"request_id" doc:"The request id to fetch the captured bodies for."`
}

type payloadGetOutput struct {
	Body payloadlog.Record
}

// --- registration ---

func registerPayloads(api huma.API, d Deps, protect huma.Middlewares) {
	if d.PayloadReader == nil {
		return
	}

	huma.Register(api, huma.Operation{
		OperationID: "payloads_list",
		Method:      http.MethodGet,
		Path:        "/payloads",
		Summary:     "List captured request/response logs (newest first), filterable",
		Description: "Lists the payload-logging captures for opted-in requests " +
			"(per-policy or per-relay-key). Returns metadata only — the bodies " +
			"are stripped; fetch them via GET /payloads/{request_id}. Filter by " +
			"time, status, and the same dimensions as /usage/events.",
		Tags:        []string{"payloads"},
		Middlewares: protect,
		Errors:      []int{400, 401, 500},
	}, func(ctx context.Context, in *payloadListInput) (*payloadListOutput, error) {
		if err := d.Authz.Authorize(ctx, "payload.list", authz.Resource{Kind: "payload"}); err != nil {
			return nil, mapAuthzErr(err)
		}
		q, err := in.toQuery()
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
		recs, err := d.PayloadReader.List(ctx, q)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if recs == nil {
			recs = []payloadlog.Record{}
		}
		out := &payloadListOutput{}
		out.Body.Records = recs
		if n := len(recs); n > 0 && n == effectiveLimit(in.Limit) {
			last := recs[n-1]
			out.Body.NextCursor = encodeCursor(last.Timestamp, last.RequestID)
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "payloads_get",
		Method:      http.MethodGet,
		Path:        "/payloads/{request_id}",
		Summary:     "Fetch the captured request + response bodies for one request",
		Description: "Returns the full captured Record — request and response " +
			"bodies included — for a single request id. 404 when no capture " +
			"exists (the request ran without payload logging opted in, or has " +
			"aged out of the store).",
		Tags:        []string{"payloads"},
		Middlewares: protect,
		Errors:      []int{401, 404, 500},
	}, func(ctx context.Context, in *payloadGetInput) (*payloadGetOutput, error) {
		if err := d.Authz.Authorize(ctx, "payload.get", authz.Resource{Kind: "payload"}); err != nil {
			return nil, mapAuthzErr(err)
		}
		rec, err := d.PayloadReader.Get(ctx, in.RequestID)
		if errors.Is(err, payloadlog.ErrNotFound) {
			return nil, huma.Error404NotFound("no captured payload for request id " + in.RequestID)
		}
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &payloadGetOutput{Body: rec}, nil
	})
}
