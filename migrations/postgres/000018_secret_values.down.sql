-- Reverse 000018. Stored rows created after the up migration carry their
-- ciphertext only in secret_values (inline columns NULL); copy it back so
-- the strict value-mode CHECK can be restored without violating rows. The
-- ciphertext is moved verbatim — no master key needed (already encrypted).
UPDATE secrets s SET
    value_ciphertext  = sv.ciphertext,
    value_nonce       = sv.nonce,
    value_key_version = sv.key_version
FROM secret_values sv
WHERE s.id = sv.id
  AND s.value_kind = 'stored'
  AND s.value_ciphertext IS NULL;

-- Restore the strict pairing (matches migration 000014's constraint).
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

DROP TABLE IF EXISTS secret_values;
