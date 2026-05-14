ALTER TABLE secrets DROP CONSTRAINT secrets_value_mode;

ALTER TABLE secrets DROP COLUMN value_key_version;

ALTER TABLE secrets ADD CONSTRAINT secrets_value_mode CHECK (
    (value_kind = 'env'    AND value_from_env IS NOT NULL AND value_ciphertext IS NULL    AND value_nonce IS NULL)
    OR
    (value_kind = 'stored' AND value_from_env IS NULL     AND value_ciphertext IS NOT NULL AND value_nonce IS NOT NULL)
);
