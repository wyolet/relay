package lastused

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/wyolet/relay/internal/storage/gen"
)

// Store writes last-used times onto the relay key and host key rows. It is the
// Flusher's Writer; the request path never calls it, and reads take the times
// from the rows through the key stores.
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

func columns(used []Entry) ([]string, []pgtype.Timestamptz) {
	ids := make([]string, len(used))
	at := make([]pgtype.Timestamptz, len(used))
	for i, e := range used {
		ids[i] = e.ID
		at[i] = pgtype.Timestamptz{Time: e.At, Valid: true}
	}
	return ids, at
}
