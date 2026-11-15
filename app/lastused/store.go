package lastused

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/wyolet/relay/internal/storage/gen"
)

// Store reads and writes the last-used tables. It is the Flusher's Writer and
// the admin read path's source; the request path never calls it.
type Store struct {
	q *gen.Queries
}

func NewStore(q *gen.Queries) *Store { return &Store{q: q} }

// TouchKeys records used for relay keys in one statement. Ids of deleted keys
// are skipped.
func (s *Store) TouchKeys(ctx context.Context, used []Entry) error {
	ids, at := columns(used)
	if err := s.q.TouchRelayKeysLastUsed(ctx, gen.TouchRelayKeysLastUsedParams{Ids: ids, UsedAt: at}); err != nil {
		return fmt.Errorf("lastused.TouchKeys: %w", err)
	}
	return nil
}

// TouchHostKeys records used for host keys in one statement. Ids of deleted
// keys are skipped.
func (s *Store) TouchHostKeys(ctx context.Context, used []Entry) error {
	ids, at := columns(used)
	if err := s.q.TouchHostKeysLastUsed(ctx, gen.TouchHostKeysLastUsedParams{Ids: ids, UsedAt: at}); err != nil {
		return fmt.Errorf("lastused.TouchHostKeys: %w", err)
	}
	return nil
}

// KeysLastUsed returns when each of the relay keys ids last served; a key
// never used has no entry.
func (s *Store) KeysLastUsed(ctx context.Context, ids []string) (map[string]time.Time, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.q.ListRelayKeysLastUsed(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("lastused.KeysLastUsed: %w", err)
	}
	out := make(map[string]time.Time, len(rows))
	for _, r := range rows {
		out[r.KeyID] = r.LastUsedAt.Time.UTC()
	}
	return out, nil
}

// HostKeysLastUsed returns when each of the host keys ids last served; a key
// never used has no entry.
func (s *Store) HostKeysLastUsed(ctx context.Context, ids []string) (map[string]time.Time, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.q.ListHostKeysLastUsed(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("lastused.HostKeysLastUsed: %w", err)
	}
	out := make(map[string]time.Time, len(rows))
	for _, r := range rows {
		out[r.HostKeyID] = r.LastUsedAt.Time.UTC()
	}
	return out, nil
}

func columns(used []Entry) ([]string, []pgtype.Timestamptz) {
	ids := make([]string, len(used))
	at := make([]pgtype.Timestamptz, len(used))
	for i, e := range used {
		ids[i] = e.ID
		at[i] = pgtype.Timestamptz{Time: e.At, Valid: true}
	}
	return ids, at
}
