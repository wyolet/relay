// store.go is the data-access layer for audit events: the Emitter's Sink
// and the read side behind GET /api/audit.
package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/internal/storage/gen"
)

// DefaultLimit and MaxLimit bound a list page.
const (
	DefaultLimit = 100
	MaxLimit     = 10000
)

// Query filters a list of audit events. Empty fields don't filter. Cursor*
// is the keyset position: rows strictly older than (CursorTS, CursorID).
type Query struct {
	ActorID       string
	ActorName     string
	Actions       []string
	ResourceKinds []string
	ResourceID    string
	Scopes        []string
	Status        string
	From          time.Time
	To            time.Time
	CursorTS      time.Time
	CursorID      string
	Limit         int
}

// Reader is the read side of the audit log.
type Reader interface {
	Events(ctx context.Context, q Query) ([]Event, error)
}

// Store reads and writes audit_events. It satisfies both Sink and Reader.
type Store struct {
	q *gen.Queries
}

// NewStore constructs a Store from an existing sqlc Queries handle.
func NewStore(q *gen.Queries) *Store { return &Store{q: q} }

// Write inserts a batch in one COPY round-trip. All or nothing: a partial
// audit batch is worse than a logged failure the drop counter already
// surfaces.
func (s *Store) Write(ctx context.Context, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	rows := make([]gen.InsertAuditEventParams, 0, len(events))
	for _, ev := range events {
		rows = append(rows, insertParams(ev))
	}
	if _, err := s.q.InsertAuditEvent(ctx, rows); err != nil {
		return fmt.Errorf("audit.Write: %w", err)
	}
	return nil
}

// Prune deletes events older than before, returning the row count.
func (s *Store) Prune(ctx context.Context, before time.Time) (int64, error) {
	n, err := s.q.PruneAuditEvents(ctx, timestamp(before))
	if err != nil {
		return 0, fmt.Errorf("audit.Prune: %w", err)
	}
	return n, nil
}

// Events returns matching events, newest first.
func (s *Store) Events(ctx context.Context, q Query) ([]Event, error) {
	rows, err := s.q.ListAuditEvents(ctx, gen.ListAuditEventsParams{
		ActorID:       text(q.ActorID),
		ActorName:     text(q.ActorName),
		Actions:       nonNil(q.Actions),
		ResourceKinds: nonNil(q.ResourceKinds),
		ResourceID:    text(q.ResourceID),
		Scopes:        nonNil(q.Scopes),
		Status:        text(q.Status),
		FromTs:        optTimestamp(q.From),
		ToTs:          optTimestamp(q.To),
		CursorTs:      optTimestamp(q.CursorTS),
		CursorID:      q.CursorID,
		RowLimit:      int32(EffectiveLimit(q.Limit)),
	})
	if err != nil {
		return nil, fmt.Errorf("audit.Events: %w", err)
	}
	out := make([]Event, 0, len(rows))
	for _, r := range rows {
		out = append(out, fromRow(r))
	}
	return out, nil
}

// EffectiveLimit clamps a caller-supplied page size.
func EffectiveLimit(limit int) int {
	switch {
	case limit <= 0:
		return DefaultLimit
	case limit > MaxLimit:
		return MaxLimit
	default:
		return limit
	}
}

func insertParams(ev Event) gen.InsertAuditEventParams {
	p := gen.InsertAuditEventParams{
		ID:           ev.ID,
		Ts:           timestamp(ev.TS),
		ActorKind:    ev.Actor.Kind,
		ActorID:      text(ev.Actor.ID),
		ActorName:    text(ev.Actor.Name),
		SessionID:    text(ev.Actor.SessionID),
		Ip:           text(ev.Actor.IP),
		Action:       ev.Action,
		ResourceKind: ev.Resource.Kind,
		ResourceID:   text(ev.Resource.ID),
		ResourceName: text(ev.Resource.Name),
		Scope:        nonNil(ev.Resource.Scope),
		Status:       ev.Outcome.Status,
		Code:         int32(ev.Outcome.Code),
		RequestID:    text(ev.Request.ID),
		Method:       text(ev.Request.Method),
		Path:         text(ev.Request.Path),
	}
	if o := ev.Resource.Owner; o != nil {
		p.OwnerKind, p.OwnerID = text(string(o.Kind)), text(o.ID)
	}
	if ev.Change != nil {
		p.ChangedFields = nonNil(ev.Change.Fields)
	}
	return p
}

func fromRow(r gen.AuditEvent) Event {
	ev := Event{
		ID: r.ID,
		TS: r.Ts.Time,
		Actor: Actor{
			Kind:      r.ActorKind,
			ID:        r.ActorID.String,
			Name:      r.ActorName.String,
			SessionID: r.SessionID.String,
			IP:        r.Ip.String,
		},
		Action: r.Action,
		Resource: Resource{
			Kind:  r.ResourceKind,
			ID:    r.ResourceID.String,
			Name:  r.ResourceName.String,
			Scope: r.Scope,
		},
		Outcome: Outcome{Status: r.Status, Code: int(r.Code)},
		Request: Request{ID: r.RequestID.String, Method: r.Method.String, Path: r.Path.String},
	}
	if r.OwnerKind.Valid && r.OwnerKind.String != "" {
		ev.Resource.Owner = &meta.Owner{Kind: meta.OwnerKind(r.OwnerKind.String), ID: r.OwnerID.String}
	}
	if r.ChangedFields != nil {
		ev.Change = &Change{Fields: r.ChangedFields}
	}
	return ev
}

func text(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

func timestamp(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func optTimestamp(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{}
	}
	return timestamp(t)
}

// nonNil keeps a nil slice out of the query params: the filters test
// cardinality(...) = 0, which a SQL NULL would not satisfy.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
