ALTER TABLE secrets DROP CONSTRAINT IF EXISTS secrets_value_mode;

ALTER TABLE secrets
    DROP COLUMN IF EXISTS value_kind,
    DROP COLUMN IF EXISTS value_from_env,
    DROP COLUMN IF EXISTS value_ciphertext,
    DROP COLUMN IF EXISTS value_nonce;
