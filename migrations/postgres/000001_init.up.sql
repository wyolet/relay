CREATE TABLE IF NOT EXISTS providers (
    name        TEXT PRIMARY KEY,
    metadata    JSONB NOT NULL,
    spec        JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS pools (
    name        TEXT PRIMARY KEY,
    metadata    JSONB NOT NULL,
    spec        JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS secrets (
    name        TEXT PRIMARY KEY,
    metadata    JSONB NOT NULL,
    spec        JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS models (
    name        TEXT PRIMARY KEY,
    metadata    JSONB NOT NULL,
    spec        JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS routes (
    name        TEXT PRIMARY KEY,
    metadata    JSONB NOT NULL,
    spec        JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS rate_limits (
    name        TEXT PRIMARY KEY,
    metadata    JSONB NOT NULL,
    spec        JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS attachments (
    parent_kind     TEXT NOT NULL,
    parent_name     TEXT NOT NULL,
    ratelimit_name  TEXT NOT NULL,
    meter           TEXT NOT NULL,
    PRIMARY KEY (parent_kind, parent_name, ratelimit_name, meter)
);

CREATE INDEX IF NOT EXISTS attachments_parent_idx ON attachments (parent_kind, parent_name);
