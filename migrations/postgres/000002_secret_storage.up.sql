-- Migration 000002: add value_kind / value_from_env / value_ciphertext / value_nonce to secrets.

ALTER TABLE secrets
    ADD COLUMN value_kind        TEXT,
    ADD COLUMN value_from_env    TEXT,
    ADD COLUMN value_ciphertext  BYTEA,
    ADD COLUMN value_nonce       BYTEA;

-- Backfill: existing rows are env-ref mode. Populate value_kind and value_from_env
-- from spec JSONB. Rows whose spec lacks valueFrom.env get value_kind='env' with a
-- NULL value_from_env — the CHECK below will reject them, surfacing the data problem.
UPDATE secrets
    SET value_kind      = 'env',
        value_from_env  = spec->'valueFrom'->>'env';

-- Now that all rows are backfilled, enforce NOT NULL and add the DEFAULT.
ALTER TABLE secrets
    ALTER COLUMN value_kind SET NOT NULL,
    ALTER COLUMN value_kind SET DEFAULT 'env';

ALTER TABLE secrets ADD CONSTRAINT secrets_value_mode CHECK (
    (value_kind = 'env'    AND value_from_env   IS NOT NULL AND value_ciphertext IS NULL    AND value_nonce IS NULL)
    OR
    (value_kind = 'stored' AND value_from_env   IS NULL     AND value_ciphertext IS NOT NULL AND value_nonce IS NOT NULL)
);
