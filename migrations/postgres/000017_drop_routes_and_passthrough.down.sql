-- Best-effort recreate. Restores the post-000008 shape both tables had
-- when they were last touched. Data is not preserved (rows were empty
-- pre-drop anyway).

CREATE TABLE IF NOT EXISTS routes (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    display_name  TEXT NOT NULL DEFAULT '',
    metadata      JSONB NOT NULL,
    spec          JSONB NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT routes_name_unique UNIQUE (name)
);

CREATE TABLE IF NOT EXISTS passthrough_config (
    name        TEXT PRIMARY KEY,
    spec        JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
