-- Batch subsystem control tables. The per-item *execution* state lives in the
-- jobq module's own tables; these own the batch concept and the batch→job
-- mapping. Payload bytes never live here (jobq stores them via its PayloadStore).
CREATE TABLE IF NOT EXISTS batches (
    id              TEXT        PRIMARY KEY,
    relay_key_hash  TEXT        NOT NULL,
    policy_id       TEXT        NOT NULL DEFAULT '',
    inbound_shape   TEXT        NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'queued',
    total_items     INTEGER     NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at    TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS batches_relay_key_idx ON batches (relay_key_hash, created_at DESC);

-- One row per submitted item: maps (batch, ordinal) → the jobq job that runs it.
-- Live item state is read from jobq by job_id; this table is the durable mapping.
CREATE TABLE IF NOT EXISTS batch_items (
    batch_id  TEXT    NOT NULL REFERENCES batches (id) ON DELETE CASCADE,
    idx       INTEGER NOT NULL,
    job_id    TEXT    NOT NULL,
    PRIMARY KEY (batch_id, idx)
);

CREATE INDEX IF NOT EXISTS batch_items_batch_idx ON batch_items (batch_id);
