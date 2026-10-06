package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wyolet/relay/pkg/payload"
	"github.com/wyolet/relay/pkg/payload/dedup"
)

// maxRecentPieces bounds the sink's memory of written pieces. Clearing it on
// overflow only costs duplicate inserts, which ReplacingMergeTree collapses.
const maxRecentPieces = 500_000

// recentPieces maps each piece this sink wrote to the UTC day it wrote it.
// A piece is re-inserted on its first sighting each day, which moves its
// last_seen forward so the pieces TTL never drops one a live request row
// references. Only insertBatch touches it, and the WAL serialises those calls.
type recentPieces struct {
	limit int
	day   map[dedup.Hash]int32
}

func newRecentPieces(limit int) *recentPieces {
	return &recentPieces{limit: limit, day: make(map[dedup.Hash]int32)}
}

func (r *recentPieces) writtenOn(h dedup.Hash, day int32) bool {
	d, ok := r.day[h]
	return ok && d == day
}

func (r *recentPieces) mark(pieces []dedup.Piece, day int32) {
	if len(r.day)+len(pieces) > r.limit {
		clear(r.day)
	}
	for _, p := range pieces {
		r.day[p.Hash] = day
	}
}

func utcDay(t time.Time) int32 {
	return int32(t.Unix() / 86400)
}

// insertDedup writes records as request rows plus the pieces not yet written
// today. Pieces go first: a failure between the two inserts leaves orphan
// pieces, never a request row pointing at a missing piece. An error leaves
// the WAL segment for retry, as in the whole-body path.
func (s *Sink) insertDedup(records []payload.Record) error {
	now := time.Now().UTC()
	today := utcDay(now)

	splits := make([]dedup.Split, len(records))
	var fresh []dedup.Piece
	queued := map[dedup.Hash]bool{}
	for i, r := range records {
		if len(r.RequestBody) > 0 {
			splits[i] = dedup.SplitBody(r.RequestBody)
		}
		for _, p := range splits[i].Pieces {
			if queued[p.Hash] || s.recent.writtenOn(p.Hash, today) {
				continue
			}
			queued[p.Hash] = true
			fresh = append(fresh, p)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if len(fresh) > 0 {
		batch, err := s.conn.PrepareBatch(ctx, insertPiecesSQL)
		if err != nil {
			return fmt.Errorf("prepare pieces batch: %w", err)
		}
		for i := range fresh {
			if err := batch.Append(fresh[i].Hash[:], string(fresh[i].Body), now); err != nil {
				return fmt.Errorf("append piece: %w", err)
			}
		}
		if err := batch.Send(); err != nil {
			return fmt.Errorf("send pieces: %w", err)
		}
		s.recent.mark(fresh, today)
	}

	batch, err := s.conn.PrepareBatch(ctx, insertRequestsSQL)
	if err != nil {
		return fmt.Errorf("prepare requests batch: %w", err)
	}
	for i, r := range records {
		if err := batch.Append(requestValues(r, splits[i])...); err != nil {
			return fmt.Errorf("append request row: %w", err)
		}
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("send requests: %w", err)
	}
	return nil
}

// requestValues returns r's payload_requests column values in
// insertRequestsSQL order. field_counts[i] is how many consecutive entries
// of hashes belong to field i.
func requestValues(r payload.Record, s dedup.Split) []any {
	names := make([]string, len(s.Fields))
	isArray := make([]uint8, len(s.Fields))
	offsets := make([]uint32, len(s.Fields))
	counts := make([]uint32, len(s.Fields))
	var hashes [][]byte
	for i, f := range s.Fields {
		names[i] = f.Name
		isArray[i] = b2u8(f.Array)
		offsets[i] = uint32(f.Offset)
		counts[i] = uint32(len(f.Hashes))
		for j := range f.Hashes {
			hashes = append(hashes, f.Hashes[j][:])
		}
	}
	return []any{
		r.RequestID, r.Timestamp, r.ProjectID, r.PrincipalID, r.RelayKeyHash,
		string(s.Skeleton), names, isArray, offsets, counts, hashes,
		string(r.ResponseBody), b2u8(r.RequestTruncated), b2u8(r.ResponseTruncated),
	}
}

var errCorruptRow = errors.New("payload/clickhouse: corrupt payload_requests row")

// fieldsFromColumns is the inverse of requestValues' field columns.
func fieldsFromColumns(names []string, isArray []uint8, offsets, counts []uint32, hashes []string) ([]dedup.Field, error) {
	if len(isArray) != len(names) || len(offsets) != len(names) || len(counts) != len(names) {
		return nil, fmt.Errorf("%w: field column lengths differ", errCorruptRow)
	}
	fields := make([]dedup.Field, len(names))
	next := 0
	for i, name := range names {
		n := int(counts[i])
		if n > len(hashes)-next {
			return nil, fmt.Errorf("%w: field counts exceed %d hashes", errCorruptRow, len(hashes))
		}
		f := dedup.Field{Name: name, Array: isArray[i] == 1, Offset: int(offsets[i]), Hashes: make([]dedup.Hash, n)}
		for j, h := range hashes[next : next+n] {
			if len(h) != len(dedup.Hash{}) {
				return nil, fmt.Errorf("%w: hash of %d bytes", errCorruptRow, len(h))
			}
			copy(f.Hashes[j][:], h)
		}
		next += n
		fields[i] = f
	}
	if next != len(hashes) {
		return nil, fmt.Errorf("%w: %d hashes, fields claim %d", errCorruptRow, len(hashes), next)
	}
	return fields, nil
}
