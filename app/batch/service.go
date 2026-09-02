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

	metaProjectID      = "project_id"
	metaTeamID         = "team_id"
	metaPrincipalKind  = "principal_kind"
	metaPrincipalID    = "principal_id"
	metaCredentialKind = "credential_kind"
	metaCredentialID   = "credential_id"
	metaPolicyID       = "policy_id"
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
	// resolveCaller is supplied by the transport; nil means every request is
	// unauthenticated.
	resolveCaller CallerFunc
}

// NewService wires the store, queue, and runner together. resolveCaller is
// how the HTTP surface learns who is asking.
func NewService(store *Store, queue *jobq.Queue, runner *Runner, resolveCaller CallerFunc) *Service {
	return &Service{store: store, queue: queue, runner: runner, resolveCaller: resolveCaller}
}

func (s *Service) caller(ctx context.Context) *Caller {
	if s.resolveCaller == nil {
		return nil
	}
	return s.resolveCaller(ctx)
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
			job.Meta(metaPolicyID),
			Attribution{
				ProjectID:      job.Meta(metaProjectID),
				TeamID:         job.Meta(metaTeamID),
				PrincipalKind:  job.Meta(metaPrincipalKind),
				PrincipalID:    job.Meta(metaPrincipalID),
				CredentialKind: job.Meta(metaCredentialKind),
				CredentialID:   job.Meta(metaCredentialID),
			},
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
//
// c is the submitting caller, recorded on the batch so every item's usage
// event carries the attribution and the policy the submission resolved to,
// rather than whatever the credential resolves to when the item finally runs.
func (s *Service) Submit(ctx context.Context, c *Caller, inbound string, items [][]byte) (string, error) {
	if len(items) == 0 {
		return "", errors.New("batch: no items")
	}
	if c == nil {
		return "", errors.New("batch: no caller")
	}
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("batch: id: %w", err)
	}
	batchID := id.String()

	b := &Batch{
		ID:           batchID,
		RelayKeyHash: c.Owner(),
		PolicyID:     c.PolicyID,
		InboundShape: inbound,
		Status:       StatusQueued,
		TotalItems:   len(items),
		Attribution:  c.Attribution,
	}
	attr := b.Attribution
	if err := s.store.Create(ctx, b); err != nil {
		return "", err
	}

	for idx, item := range items {
		jobID, err := s.queue.Enqueue(ctx, item, jobq.EnqueueOpts{
			Queue:       Queue,
			MaxAttempts: 1, // pipeline already fails over across keys; a hard failure shouldn't replay the whole item
			Metadata: map[string]string{
				metaBatchID:  batchID,
				metaItemIdx:  strconv.Itoa(idx),
				metaKeyHash:  c.KeyHash,
				metaInbound:  inbound,
				metaPolicyID: c.PolicyID,
				// Carried per item so execution reads the submission's
				// attribution without a second trip to the batch row.
				metaProjectID:      attr.ProjectID,
				metaTeamID:         attr.TeamID,
				metaPrincipalKind:  attr.PrincipalKind,
				metaPrincipalID:    attr.PrincipalID,
				metaCredentialKind: attr.CredentialKind,
				metaCredentialID:   attr.CredentialID,
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
// owner must match the batch owner token.
func (s *Service) Status(ctx context.Context, id, owner string) (*BatchView, error) {
	b, err := s.owned(ctx, id, owner)
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
func (s *Service) Results(ctx context.Context, id, owner string) ([]ItemResult, error) {
	if _, err := s.owned(ctx, id, owner); err != nil {
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
// owner must match the batch owner token.
func (s *Service) Cancel(ctx context.Context, id, owner string) error {
	if _, err := s.owned(ctx, id, owner); err != nil {
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

// owned fetches a batch and verifies the caller owns it. An empty owner
// token never matches: a caller with no identity owns nothing.
func (s *Service) owned(ctx context.Context, id, owner string) (*Batch, error) {
	b, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if owner == "" || b.RelayKeyHash != owner {
		return nil, ErrForbidden
	}
	return b, nil
}
