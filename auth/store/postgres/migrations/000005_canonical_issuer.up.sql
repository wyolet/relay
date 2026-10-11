-- Identities are keyed by the canonical issuer, without trailing slashes: a provider and a configuration may disagree on the slash, and a lookup made with either form must find the account either one created.
--
-- users.oidc_subject keeps the issuer as it was written. A binary from before the identities table looks accounts up by that column with its own configured issuer, matching it exactly, so rewriting the column would hide every account from it on a rollback.

-- Identities that differ only by trailing slashes become one, and the oldest row keeps it. Any other account among them keeps its oidc_subject but no longer signs in with that identity.
DELETE FROM identities i
 USING (SELECT issuer, subject,
               row_number() OVER (PARTITION BY rtrim(issuer, '/'), subject ORDER BY created_at, issuer) AS n
          FROM identities) r
 WHERE i.issuer = r.issuer AND i.subject = r.subject AND r.n > 1;

UPDATE identities SET issuer = rtrim(issuer, '/') WHERE issuer <> rtrim(issuer, '/');

CREATE OR REPLACE FUNCTION mirror_primary_identity() RETURNS trigger AS $fn$
DECLARE
    sep         INT;
    old_issuer  TEXT;
    old_subject TEXT;
    new_issuer  TEXT;
    new_subject TEXT;
BEGIN
    sep := strpos(NEW.oidc_subject, '|');
    IF sep > 0 THEN
        new_issuer  := rtrim(left(NEW.oidc_subject, sep - 1), '/');
        new_subject := substr(NEW.oidc_subject, sep + 1);
    END IF;
    IF TG_OP = 'UPDATE' THEN
        sep := strpos(OLD.oidc_subject, '|');
        IF sep > 0 THEN
            old_issuer  := rtrim(left(OLD.oidc_subject, sep - 1), '/');
            old_subject := substr(OLD.oidc_subject, sep + 1);
        END IF;
        -- A value that changes only the issuer's trailing slash names the same identity, whose row and external id stay.
        IF old_issuer IS NOT DISTINCT FROM new_issuer AND old_subject IS NOT DISTINCT FROM new_subject THEN
            RETURN NULL;
        END IF;
        IF old_issuer IS NOT NULL THEN
            DELETE FROM identities WHERE user_id = OLD.id AND issuer = old_issuer AND subject = old_subject;
        END IF;
    END IF;
    -- An identity another account holds is left to the primary key to refuse.
    IF new_issuer IS NOT NULL AND NOT EXISTS (SELECT 1 FROM identities
                                               WHERE user_id = NEW.id AND issuer = new_issuer AND subject = new_subject) THEN
        INSERT INTO identities (user_id, issuer, subject) VALUES (NEW.id, new_issuer, new_subject);
    END IF;
    RETURN NULL;
END;
$fn$ LANGUAGE plpgsql;
