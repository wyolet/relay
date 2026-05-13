-- 000009: app/ architecture additions.
--
-- Additive only — internal/ keeps working until cutover.
--   1. hosts:           new table for serving endpoints (Provider/Host split).
--   2. policy_models:   junction replacing policies.spec.modelIds JSONB array.
--   3. policy_host_keys: junction replacing policies.spec.hostKeyIds JSONB array.
--   4. policies.rate_limit_id: nullable FK replacing policies.spec.rateLimitId.
--
-- HostKey rows continue to live in the existing `secrets` table (owner.kind
-- distinguishes them); no new table is required.

CREATE TABLE IF NOT EXISTS hosts (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL DEFAULT '',
    metadata     JSONB NOT NULL,
    spec         JSONB NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS policy_models (
    policy_id TEXT NOT NULL REFERENCES policies(id) ON DELETE CASCADE,
    model_id  TEXT NOT NULL REFERENCES models(id)   ON DELETE RESTRICT,
    position  INT  NOT NULL,
    PRIMARY KEY (policy_id, model_id)
);
CREATE INDEX IF NOT EXISTS policy_models_model_idx ON policy_models (model_id);

CREATE TABLE IF NOT EXISTS policy_host_keys (
    policy_id   TEXT NOT NULL REFERENCES policies(id) ON DELETE CASCADE,
    host_key_id TEXT NOT NULL REFERENCES secrets(id)  ON DELETE RESTRICT,
    position    INT  NOT NULL,
    PRIMARY KEY (policy_id, host_key_id)
);
CREATE INDEX IF NOT EXISTS policy_host_keys_key_idx ON policy_host_keys (host_key_id);

ALTER TABLE policies
    ADD COLUMN IF NOT EXISTS rate_limit_id TEXT REFERENCES rate_limits(id) ON DELETE SET NULL;
