package pseudo

import (
	"context"
	"sync"
)

// reads records the rows a pseudo write read from the database while it expanded and checked its rows. They are the current truth the live plans may not have seen yet, so the plan check lays them over the live view, under the rows being written.
type reads struct {
	mu   sync.Mutex
	rows []readRow
}

type readRow struct {
	id  string
	row any
}

type readsKey struct{}

func withReads(ctx context.Context) (context.Context, *reads) {
	r := &reads{}
	return context.WithValue(ctx, readsKey{}, r), r
}

// Record keeps row, read under id, for the plan check of the pseudo write ctx belongs to; a no-op outside one and for a nil row. A kind's expansion and the checks its rows pass call it for every row they read from a store.
func Record[T any](ctx context.Context, id string, row *T) {
	r, _ := ctx.Value(readsKey{}).(*reads)
	if r == nil || row == nil || id == "" {
		return
	}
	r.mu.Lock()
	r.rows = append(r.rows, readRow{id: id, row: row})
	r.mu.Unlock()
}

// except returns the rows read, in read order, leaving out those whose id is in created, the write's own new rows read back while later rows were checked, and keeping of an id in changed only the first read, the row as stored before a step updated or deleted it.
func (r *reads) except(created, changed map[string]bool) []any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]any, 0, len(r.rows))
	seen := map[string]bool{}
	for _, rr := range r.rows {
		if created[rr.id] || (changed[rr.id] && seen[rr.id]) {
			continue
		}
		seen[rr.id] = true
		out = append(out, rr.row)
	}
	return out
}
