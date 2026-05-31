package jobq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// store is the Postgres persistence layer. All SQL lives here. The job row is a
// lean envelope — never payload bytes — so the claim query can SELECT the whole
// row cheaply.
type store struct {
	pool *pgxpool.Pool
}

// jobColumns is the single source of truth for the column set and order; both
// the bare (jobCols) and table-qualified (jobColsJ) forms derive from it, and
// scanJob scans in exactly this order.
var jobColumns = []string{
	"id", "queue", "state", "priority", "attempt", "max_attempts",
	"input_uri", "result_uri", "metadata", "last_error",
	"scheduled_at", "attempted_at", "finalized_at", "created_at",
}

// jobCols is the bare list for plain SELECT ... FROM jobq_jobs.
var jobCols = strings.Join(jobColumns, ", ")

// jobColsJ is the j-qualified list for the claim's UPDATE ... FROM ... RETURNING,
// where an unqualified "id" would be ambiguous across jobq_jobs j and the CTE.
var jobColsJ = func() string {
	q := make([]string, len(jobColumns))
	for i, c := range jobColumns {
		q[i] = "j." + c
	}
	return strings.Join(q, ", ")
}()

func (s *store) insert(ctx context.Context, j *Job) error {
	meta, err := marshalMeta(j.Metadata)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO jobq_jobs
			(id, queue, state, priority, attempt, max_attempts,
			 input_uri, result_uri, metadata, last_error, scheduled_at)
		VALUES ($1,$2,$3,$4,0,$5,$6,'',$7::jsonb,'',$8)`,
		j.ID, j.Queue, j.State, j.Priority, j.MaxAttempts, j.InputURI, meta, j.ScheduledAt)
	if err != nil {
		return fmt.Errorf("jobq: insert: %w", err)
	}
	return nil
}

// claimOne atomically claims the highest-priority due job in queue, flipping it
// to 'running' and stamping the attempt. Returns nil when nothing is available.
// The claim commits immediately — no transaction is held while the job runs.
func (s *store) claimOne(ctx context.Context, queue string) (*Job, error) {
	row := s.pool.QueryRow(ctx, `
		WITH locked AS (
			SELECT id FROM jobq_jobs
			WHERE state = 'available' AND queue = $1 AND scheduled_at <= NOW()
			ORDER BY priority ASC, scheduled_at ASC, id ASC
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE jobq_jobs j
		SET state = 'running', attempt = j.attempt + 1, attempted_at = NOW()
		FROM locked
		WHERE j.id = locked.id
		RETURNING `+jobColsJ, queue)
	j, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("jobq: claim: %w", err)
	}
	return j, nil
}

func (s *store) get(ctx context.Context, id string) (*Job, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+jobCols+` FROM jobq_jobs WHERE id = $1`, id)
	j, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("jobq: get: %w", err)
	}
	return j, nil
}

func (s *store) markCompleted(ctx context.Context, id, resultURI string) error {
	return s.exec(ctx, "complete", `
		UPDATE jobq_jobs
		SET state = 'completed', result_uri = $2, last_error = '', finalized_at = NOW()
		WHERE id = $1 AND state = 'running'`, id, resultURI)
}

func (s *store) markRetryable(ctx context.Context, id string, next time.Time, lastErr string) error {
	return s.exec(ctx, "retry", `
		UPDATE jobq_jobs
		SET state = 'retryable', scheduled_at = $2, last_error = $3
		WHERE id = $1 AND state = 'running'`, id, next, lastErr)
}

func (s *store) markDiscarded(ctx context.Context, id, lastErr string) error {
	return s.exec(ctx, "discard", `
		UPDATE jobq_jobs
		SET state = 'discarded', last_error = $2, finalized_at = NOW()
		WHERE id = $1 AND state = 'running'`, id, lastErr)
}

// markRequeue returns a running job to 'available' without consuming the
// attempt — used when a graceful shutdown interrupts a job mid-flight (not a
// failure, so it shouldn't count against MaxAttempts).
func (s *store) markRequeue(ctx context.Context, id string) error {
	return s.exec(ctx, "requeue", `
		UPDATE jobq_jobs
		SET state = 'available', attempt = GREATEST(attempt - 1, 0), scheduled_at = NOW()
		WHERE id = $1 AND state = 'running'`, id)
}

// markCancelledRunning finalizes a running job as cancelled (caller-requested).
func (s *store) markCancelledRunning(ctx context.Context, id, lastErr string) error {
	return s.exec(ctx, "cancel-running", `
		UPDATE jobq_jobs
		SET state = 'cancelled', last_error = $2, finalized_at = NOW()
		WHERE id = $1 AND state = 'running'`, id, lastErr)
}

// cancelPending cancels a not-yet-running job (available/scheduled/retryable).
// Returns true if a row was cancelled.
func (s *store) cancelPending(ctx context.Context, id string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE jobq_jobs
		SET state = 'cancelled', finalized_at = NOW()
		WHERE id = $1 AND state IN ('available', 'scheduled', 'retryable')`, id)
	if err != nil {
		return false, fmt.Errorf("jobq: cancel pending: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// promoteDue moves due scheduled/retryable jobs to available. Returns the count
// promoted.
func (s *store) promoteDue(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE jobq_jobs
		SET state = 'available'
		WHERE state IN ('scheduled', 'retryable') AND scheduled_at <= NOW()`)
	if err != nil {
		return 0, fmt.Errorf("jobq: promote: %w", err)
	}
	return tag.RowsAffected(), nil
}

// rescueStuck reclaims running jobs whose attempted_at predates horizon — their
// worker is presumed dead. Each goes to 'retryable' (attempts remain) or
// 'discarded' (exhausted). Uses SKIP LOCKED so concurrent rescuers on multiple
// nodes are harmless. Returns the count rescued.
func (s *store) rescueStuck(ctx context.Context, horizon time.Time, limit int) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		WITH stuck AS (
			SELECT id FROM jobq_jobs
			WHERE state = 'running' AND attempted_at < $1
			ORDER BY attempted_at ASC
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		UPDATE jobq_jobs j
		SET state        = CASE WHEN j.attempt >= j.max_attempts THEN 'discarded' ELSE 'retryable' END,
		    finalized_at = CASE WHEN j.attempt >= j.max_attempts THEN NOW() ELSE NULL END,
		    scheduled_at = CASE WHEN j.attempt >= j.max_attempts THEN j.scheduled_at ELSE NOW() END,
		    last_error   = 'jobq: rescued — worker presumed dead'
		FROM stuck
		WHERE j.id = stuck.id`, horizon, limit)
	if err != nil {
		return 0, fmt.Errorf("jobq: rescue: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (s *store) exec(ctx context.Context, op, sql string, args ...any) error {
	if _, err := s.pool.Exec(ctx, sql, args...); err != nil {
		return fmt.Errorf("jobq: %s: %w", op, err)
	}
	return nil
}

// scanner is satisfied by both pgx.Row and pgx.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanJob(s scanner) (*Job, error) {
	var (
		j        Job
		state    string
		meta     []byte
		attempt  *time.Time
		finalize *time.Time
	)
	if err := s.Scan(
		&j.ID, &j.Queue, &state, &j.Priority, &j.Attempt, &j.MaxAttempts,
		&j.InputURI, &j.ResultURI, &meta, &j.LastError,
		&j.ScheduledAt, &attempt, &finalize,
		&j.CreatedAt,
	); err != nil {
		return nil, err
	}
	j.State = State(state)
	j.AttemptedAt = attempt
	j.FinalizedAt = finalize
	if len(meta) > 0 {
		if err := json.Unmarshal(meta, &j.Metadata); err != nil {
			return nil, fmt.Errorf("jobq: decode metadata: %w", err)
		}
	}
	return &j, nil
}

func marshalMeta(m map[string]string) ([]byte, error) {
	if len(m) == 0 {
		return []byte("{}"), nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("jobq: encode metadata: %w", err)
	}
	return b, nil
}
