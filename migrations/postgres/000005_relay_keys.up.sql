CREATE TABLE IF NOT EXISTS relay_keys (
    name        TEXT PRIMARY KEY,
    key_hash    TEXT NOT NULL,
    metadata    JSONB NOT NULL,
    spec        JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS relay_keys_key_hash_idx ON relay_keys (key_hash);
