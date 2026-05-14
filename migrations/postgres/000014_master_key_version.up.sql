-- Migration 000014: track which master key version encrypted each stored secret.
--
-- Stored-mode rows get value_key_version (starts at 1 on first encryption,
-- bumps on each /master-key/rotate). Env-mode rows have no ciphertext and
-- therefore no version (NULL). The CHECK constraint enforces the pairing.

ALTER TABLE secrets ADD COLUMN value_key_version INT;

UPDATE secrets SET value_key_version = 1 WHERE value_kind = 'stored';

ALTER TABLE secrets DROP CONSTRAINT secrets_value_mode;

ALTER TABLE secrets ADD CONSTRAINT secrets_value_mode CHECK (
    (value_kind = 'env'
        AND value_from_env    IS NOT NULL
        AND value_ciphertext  IS NULL
        AND value_nonce       IS NULL
        AND value_key_version IS NULL)
    OR
    (value_kind = 'stored'
        AND value_from_env    IS NULL
        AND value_ciphertext  IS NOT NULL
        AND value_nonce       IS NOT NULL
        AND value_key_version IS NOT NULL)
);
