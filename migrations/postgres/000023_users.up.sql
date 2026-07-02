-- 000023: DB-backed users.
--
-- Users are NOT a catalog kind: no metadata JSONB, no NOTIFY trigger, no
-- seed-managed rows. The YAML identity files (config/users/) become a
-- seed-if-absent bootstrap into this table at boot; login reads the table.
--
-- password_hash is NULL for users provisioned via an external identity
-- provider (they have no local password). oidc_subject is "issuer|sub",
-- NULL for password-only users. PG unique allows multiple NULLs.

CREATE TABLE IF NOT EXISTS users (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    email         TEXT UNIQUE,
    password_hash TEXT,
    oidc_subject  TEXT UNIQUE,
    roles         TEXT[] NOT NULL DEFAULT '{}',
    disabled      BOOLEAN NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
