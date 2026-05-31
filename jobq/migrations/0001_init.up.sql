CREATE TABLE IF NOT EXISTS jobq_jobs (
    id            TEXT        PRIMARY KEY,
    queue         TEXT        NOT NULL DEFAULT 'default',
    state         TEXT        NOT NULL,
    priority      INTEGER     NOT NULL DEFAULT 0,
    attempt       INTEGER     NOT NULL DEFAULT 0,
    max_attempts  INTEGER     NOT NULL DEFAULT 1,
    input_uri     TEXT        NOT NULL DEFAULT '',
    result_uri    TEXT        NOT NULL DEFAULT '',
    metadata      JSONB       NOT NULL DEFAULT '{}',
    last_error    TEXT        NOT NULL DEFAULT '',
    scheduled_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    attempted_at  TIMESTAMPTZ,
    finalized_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- The load-bearing invariant: finalized_at is set if and only if the job
    -- is in a terminal state. Guards against half-finalized rows.
    CONSTRAINT jobq_finalized_iff_terminal CHECK (
        (state IN ('completed', 'discarded', 'cancelled')) = (finalized_at IS NOT NULL)
    )
);

-- Claim path: only 'available' rows, ordered by (priority, scheduled_at, id).
-- Partial index keeps it tiny and hot regardless of completed-job volume.
CREATE INDEX IF NOT EXISTS jobq_jobs_claim_idx
    ON jobq_jobs (queue, priority, scheduled_at, id)
    WHERE state = 'available';

-- Scheduler path: promote due 'scheduled'/'retryable' rows to 'available'.
CREATE INDEX IF NOT EXISTS jobq_jobs_schedule_idx
    ON jobq_jobs (scheduled_at)
    WHERE state IN ('scheduled', 'retryable');

-- Rescuer path: find 'running' rows whose worker presumably died.
CREATE INDEX IF NOT EXISTS jobq_jobs_rescue_idx
    ON jobq_jobs (attempted_at)
    WHERE state = 'running';
