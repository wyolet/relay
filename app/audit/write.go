package audit

import (
	"context"
	"net/http"
	"time"

	"github.com/wyolet/relay/pkg/ids"
)

// WriteRows writes rows attributed to actor straight to sink, for an action taken outside an HTTP request (an offline relay command). Unlike Emit it waits for the write, so the caller can tell its operator that the record is missing. A row without a Code records 200.
func WriteRows(ctx context.Context, sink Sink, actor Actor, req Request, rows []Row) error {
	if len(rows) == 0 {
		return nil
	}
	now := time.Now().UTC()
	events := make([]Event, 0, len(rows))
	for _, r := range rows {
		status := r.Status
		if status == "" {
			status = StatusAllowed
		}
		code := r.Code
		if code == 0 {
			code = http.StatusOK
		}
		events = append(events, Event{
			ID: ids.New(), TS: now, Actor: actor, Action: r.Action, Resource: r.Resource,
			Outcome: Outcome{Status: status, Code: code}, Request: req, Change: changeOf(r.Fields),
		})
	}
	return sink.Write(ctx, events)
}
