CREATE TABLE IF NOT EXISTS passthrough_config (
    name        TEXT PRIMARY KEY,
    spec        JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
