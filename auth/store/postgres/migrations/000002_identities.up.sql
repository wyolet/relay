-- Sign-in identities move to a table of their own, so an account can hold several, and users and groups gain the provisioning source that created them and its id there.
--
-- users.oidc_subject ("issuer|subject", split at the first "|") stays and holds the account's primary identity: a binary from before this migration still reads it. The trigger mirrors every write of the column into identities, so accounts such a binary writes during a rollback stay reachable once the newer binary is back. A value without a separator names no issuer and gets no row.

CREATE TABLE IF NOT EXISTS identities (
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    issuer      TEXT NOT NULL,
    subject     TEXT NOT NULL,
    external_id TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (issuer, subject),
    UNIQUE (issuer, external_id)
);
CREATE INDEX IF NOT EXISTS identities_user_idx ON identities (user_id);

INSERT INTO identities (user_id, issuer, subject, created_at)
SELECT id, left(oidc_subject, strpos(oidc_subject, '|') - 1), substr(oidc_subject, strpos(oidc_subject, '|') + 1), created_at
  FROM users
 WHERE strpos(oidc_subject, '|') > 0
ON CONFLICT DO NOTHING;

CREATE OR REPLACE FUNCTION mirror_primary_identity() RETURNS trigger AS $fn$
DECLARE
    sep INT;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        IF NEW.oidc_subject IS NOT DISTINCT FROM OLD.oidc_subject THEN
            RETURN NULL;
        END IF;
        sep := strpos(OLD.oidc_subject, '|');
        IF sep > 0 THEN
            DELETE FROM identities
             WHERE user_id = OLD.id AND issuer = left(OLD.oidc_subject, sep - 1) AND subject = substr(OLD.oidc_subject, sep + 1);
        END IF;
    END IF;
    sep := strpos(NEW.oidc_subject, '|');
    -- An identity another account holds is left to the primary key to refuse.
    IF sep > 0 AND NOT EXISTS (SELECT 1 FROM identities
                                WHERE user_id = NEW.id AND issuer = left(NEW.oidc_subject, sep - 1)
                                  AND subject = substr(NEW.oidc_subject, sep + 1)) THEN
        INSERT INTO identities (user_id, issuer, subject)
        VALUES (NEW.id, left(NEW.oidc_subject, sep - 1), substr(NEW.oidc_subject, sep + 1));
    END IF;
    RETURN NULL;
END;
$fn$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS users_primary_identity ON users;
CREATE TRIGGER users_primary_identity AFTER INSERT OR UPDATE OF oidc_subject ON users
    FOR EACH ROW EXECUTE FUNCTION mirror_primary_identity();

-- The source scopes the id; NULLs are distinct, so a source may hold rows it sent no id for.
ALTER TABLE users  ADD COLUMN IF NOT EXISTS external_source TEXT, ADD COLUMN IF NOT EXISTS external_id TEXT;
ALTER TABLE users  ADD CONSTRAINT users_external_key UNIQUE (external_source, external_id);
ALTER TABLE groups ADD COLUMN IF NOT EXISTS external_source TEXT, ADD COLUMN IF NOT EXISTS external_id TEXT;
ALTER TABLE groups ADD CONSTRAINT groups_external_key UNIQUE (external_source, external_id);
