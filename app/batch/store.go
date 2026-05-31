package batch

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/internal/storage/gen"
)

// ErrNotFound is returned when a batch id is unknown.
var ErrNotFound = errors.New("batch: not found")

// Store persists batch records and the batch→job mapping via sqlc-generated
// queries. Per-item execution state is NOT stored here — it is read from jobq
// by job id.
type Store struct {
	q *gen.Queries
}

// NewStore constructs a Store over pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{q: gen.New(pool)} }

// Create inserts a new batch record (status, counts; items added separately).
func (s *Store) Create(ctx context.Context, b *Batch) error {
	if err := s.q.CreateBatch(ctx, gen.CreateBatchParams{
		ID:           b.ID,
		RelayKeyHash: b.RelayKeyHash,
		PolicyID:     b.PolicyID,
		InboundShape: b.InboundShape,
		Status:       string(b.Status),
		TotalItems:   int32(b.TotalItems),
	}); err != nil {
		return fmt.Errorf("batch: create: %w", err)
	}
	return nil
}

// Get returns a batch by id, or ErrNotFound.
func (s *Store) Get(ctx context.Context, id string) (*Batch, error) {
	row, err := s.q.GetBatch(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("batch: get: %w", err)
	}
	return toBatch(row), nil
}

// ListByRelayKey returns a caller's batches, newest first.
func (s *Store) ListByRelayKey(ctx context.Context, relayKeyHash string) ([]*Batch, error) {
	rows, err := s.q.ListBatchesByRelayKey(ctx, relayKeyHash)
	if err != nil {
		return nil, fmt.Errorf("batch: list: %w", err)
	}
	out := make([]*Batch, len(rows))
	for i, r := range rows {
		out[i] = toBatch(r)
	}
	return out, nil
}

// SetStatus updates the cached status.
func (s *Store) SetStatus(ctx context.Context, id string, st Status) error {
	if err := s.q.SetBatchStatus(ctx, gen.SetBatchStatusParams{ID: id, Status: string(st)}); err != nil {
		return fmt.Errorf("batch: set status: %w", err)
	}
	return nil
}

// SetCompleted stamps the terminal status and completed_at.
func (s *Store) SetCompleted(ctx context.Context, id string, st Status) error {
	if err := s.q.SetBatchCompleted(ctx, gen.SetBatchCompletedParams{ID: id, Status: string(st)}); err != nil {
		return fmt.Errorf("batch: set completed: %w", err)
	}
	return nil
}

// AddItem records the (batch, ordinal) → jobq job mapping.
func (s *Store) AddItem(ctx context.Context, batchID string, idx int, jobID string) error {
	if err := s.q.CreateBatchItem(ctx, gen.CreateBatchItemParams{
		BatchID: batchID,
		Idx:     int32(idx),
		JobID:   jobID,
	}); err != nil {
		return fmt.Errorf("batch: add item: %w", err)
	}
	return nil
}

// Items returns a batch's items in ordinal order.
func (s *Store) Items(ctx context.Context, batchID string) ([]Item, error) {
	rows, err := s.q.ListBatchItems(ctx, batchID)
	if err != nil {
		return nil, fmt.Errorf("batch: items: %w", err)
	}
	out := make([]Item, len(rows))
	for i, r := range rows {
		out[i] = Item{BatchID: r.BatchID, Idx: int(r.Idx), JobID: r.JobID}
	}
	return out, nil
}

func toBatch(r gen.Batch) *Batch {
	b := &Batch{
		ID:           r.ID,
		RelayKeyHash: r.RelayKeyHash,
		PolicyID:     r.PolicyID,
		InboundShape: r.InboundShape,
		Status:       Status(r.Status),
		TotalItems:   int(r.TotalItems),
		CreatedAt:    r.CreatedAt.Time,
	}
	if r.CompletedAt.Valid {
		t := r.CompletedAt.Time
		b.CompletedAt = &t
	}
	return b
}
