-- Undoes 000005: the trigger mirrors users.oidc_subject verbatim again, and each primary identity gets back the issuer as oidc_subject spells it, the form a binary before 000005 looks up exactly. Other identities keep the canonical issuer; their written form was not kept.
UPDATE identities i
   SET issuer = left(u.oidc_subject, strpos(u.oidc_subject, '|') - 1)
  FROM users u
 WHERE u.id = i.user_id
   AND strpos(u.oidc_subject, '|') > 0
   AND i.issuer = rtrim(left(u.oidc_subject, strpos(u.oidc_subject, '|') - 1), '/')
   AND i.subject = substr(u.oidc_subject, strpos(u.oidc_subject, '|') + 1)
   AND i.issuer <> left(u.oidc_subject, strpos(u.oidc_subject, '|') - 1);

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
