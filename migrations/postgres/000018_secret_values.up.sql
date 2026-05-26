-- secret_values is the generic stored-secret value store behind
-- pkg/secret's "stored" backend: AES-GCM ciphertext addressed by an opaque
-- id, decoupled from any owning entity. Used by HostKey (id = HostKey id)
-- and by the relay's own native secrets (e.g. object-store keys).
CREATE TABLE IF NOT EXISTS secret_values (
    id           TEXT PRIMARY KEY,
    ciphertext   BYTEA       NOT NULL,
    nonce        BYTEA       NOT NULL,
    key_version  INT         NOT NULL DEFAULT 1,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Migrate existing stored-mode HostKey ciphertext into the generic store,
-- keyed by the HostKey id. The source columns on `secrets`
-- (value_ciphertext / value_nonce / value_key_version) are deliberately
-- RETAINED here (nullable) so this migration is reversible without data
-- loss; a later migration drops them once the new path is verified in prod.
INSERT INTO secret_values (id, ciphertext, nonce, key_version)
SELECT id, value_ciphertext, value_nonce, COALESCE(value_key_version, 1)
FROM secrets
WHERE value_kind = 'stored' AND value_ciphertext IS NOT NULL
ON CONFLICT (id) DO NOTHING;

-- Relax the value-mode CHECK: stored rows no longer carry inline ciphertext
-- (it lives in secret_values now), so new stored rows leave
-- value_ciphertext/nonce/key_version NULL. Keep the env invariant (env needs
-- value_from_env; stored must not set it). Migrated rows keep their inline
-- columns until the later drop migration — the relaxed CHECK permits both.
ALTER TABLE secrets DROP CONSTRAINT secrets_value_mode;
ALTER TABLE secrets ADD CONSTRAINT secrets_value_mode CHECK (
    (value_kind = 'env'    AND value_from_env IS NOT NULL)
    OR
    (value_kind = 'stored' AND value_from_env IS NULL)
);
