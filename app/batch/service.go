package batch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/jobq"
)

// Queue is the jobq queue name batch items run on. The Service registers its
// handler under this name; the worker pool drains it.
const Queue = "inference"

// Job metadata keys carried on each enqueued item (opaque to jobq).
const (
	metaBatchID = "batch_id"
	metaItemIdx = "item_idx"
	metaKeyHash = "relay_key_hash"
	metaInbound = "inbound_shape"
)

// ErrForbidden is returned when a caller asks about a batch they don't own.
var ErrForbidden = errors.New("batch: not owner")

// Service is the batch subsystem's application layer: it accepts submissions,
// enqueues each item as a jobq job, and answers status/results/cancel. It is a
// pure consumer of jobq (execution) + Store (the batch record).
type Service struct {
	store  *Store
	queue  *jobq.Queue
	runner *Runner
}

// NewService wires the store, queue, and runner together.
func NewService(store *Store, queue *jobq.Queue, runner *Runner) *Service {
	return &Service{store: store, queue: queue, runner: runner}
}

// Handler is the jobq Handler that runs one batch item. Registered on the
// queue at boot. Pure input→output: it runs the item through the realtime
// pipeline (reusing keypool/breakers/usage) and returns the response bytes,
// which jobq persists as the item's result. A non-2xx upstream status is a
// failure so jobq records it (the body is not retained in v1 — follow-up).
func (s *Service) Handler() jobq.Handler {
	return func(ctx context.Context, job *jobq.Job) ([]byte, error) {
		status, out, err := s.runner.Run(
			ctx,
			job.ID,
			job.Meta(metaKeyHash),
			adapters.Name(job.Meta(metaInbound)),
			job.Input(),
		)
		if err != nil {
			return nil, err
		}
		if status < 200 || status >= 300 {
			return nil, fmt.Errorf("batch item: upstream status %d", status)
		}
		return out, nil
	}
}

// Submit creates a batch and enqueues one job per item. items are raw request
// bodies in the given inbound shape. Returns the new batch id.
func (s *Service) Submit(ctx context.Context, relayKeyHash, policyID, inbound string, items [][]byte) (string, error) {
	if len(items) == 0 {
		return "", errors.New("batch: no items")
	}
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("batch: id: %w", err)
	}
	batchID := id.String()

	if err := s.store.Create(ctx, &Batch{
		ID:           batchID,
		RelayKeyHash: relayKeyHash,
		PolicyID:     policyID,
		InboundShape: inbound,
		Status:       StatusQueued,
		TotalItems:   len(items),
	}); err != nil {
		return "", err
	}

	for idx, item := range items {
		jobID, err := s.queue.Enqueue(ctx, item, jobq.EnqueueOpts{
			Queue:       Queue,
			MaxAttempts: 1, // pipeline already fails over across keys; a hard failure shouldn't replay the whole item
			Metadata: map[string]string{
				metaBatchID: batchID,
				metaItemIdx: strconv.Itoa(idx),
				metaKeyHash: relayKeyHash,
				metaInbound: inbound,
			},
		})
		if err != nil {
			return "", fmt.Errorf("batch: enqueue item %d: %w", idx, err)
		}
		if err := s.store.AddItem(ctx, batchID, idx, jobID); err != nil {
			return "", fmt.Errorf("batch: map item %d: %w", idx, err)
		}
	}
	return batchID, nil
}

// ItemView is one item's live state, read from jobq.
type ItemView struct {
	Idx   int        `json:"idx"`
	State jobq.State `json:"state"`
}

// BatchView is a batch plus its items' live states and a per-state roll-up.
type BatchView struct {
	*Batch
	Counts map[jobq.State]int `json:"counts"`
	Items  []ItemView         `json:"items"`
}

// Status returns the batch with live per-item states aggregated from jobq.
// relayKeyHash must match the batch owner.
func (s *Service) Status(ctx context.Context, id, relayKeyHash string) (*BatchView, error) {
	b, err := s.owned(ctx, id, relayKeyHash)
	if err != nil {
		return nil, err
	}
	items, err := s.store.Items(ctx, id)
	if err != nil {
		return nil, err
	}
	view := &BatchView{Batch: b, Counts: map[jobq.State]int{}}
	for _, it := range items {
		state := jobq.State("unknown")
		if j, err := s.queue.Get(ctx, it.JobID); err == nil {
			state = j.State
		}
		view.Counts[state]++
		view.Items = append(view.Items, ItemView{Idx: it.Idx, State: state})
	}
	return view, nil
}

// ItemResult is one item's terminal outcome for the results endpoint.
type ItemResult struct {
	Idx      int             `json:"idx"`
	State    jobq.State      `json:"state"`
	Response json.RawMessage `json:"response,omitempty"`
	Error    string          `json:"error,omitempty"`
}

// Results returns each item's outcome: the response body for completed items,
// the error for failed ones. relayKeyHash must match the batch owner.
func (s *Service) Results(ctx context.Context, id, relayKeyHash string) ([]ItemResult, error) {
	if _, err := s.owned(ctx, id, relayKeyHash); err != nil {
		return nil, err
	}
	items, err := s.store.Items(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]ItemResult, 0, len(items))
	for _, it := range items {
		res := ItemResult{Idx: it.Idx, State: jobq.State("unknown")}
		j, err := s.queue.Get(ctx, it.JobID)
		if err == nil {
			res.State = j.State
			res.Error = j.LastError
		}
		if res.State == jobq.StateCompleted {
			if body, err := s.queue.Result(ctx, it.JobID); err == nil {
				res.Response = json.RawMessage(body)
			}
		}
		out = append(out, res)
	}
	return out, nil
}

// Cancel cancels every not-yet-terminal item and marks the batch cancelled.
// relayKeyHash must match the batch owner.
func (s *Service) Cancel(ctx context.Context, id, relayKeyHash string) error {
	if _, err := s.owned(ctx, id, relayKeyHash); err != nil {
		return err
	}
	items, err := s.store.Items(ctx, id)
	if err != nil {
		return err
	}
	for _, it := range items {
		_, _ = s.queue.Cancel(ctx, it.JobID)
	}
	return s.store.SetCompleted(ctx, id, StatusCancelled)
}

// owned fetches a batch and verifies the caller owns it.
func (s *Service) owned(ctx context.Context, id, relayKeyHash string) (*Batch, error) {
	b, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if b.RelayKeyHash != relayKeyHash {
		return nil, ErrForbidden
	}
	return b, nil
}
