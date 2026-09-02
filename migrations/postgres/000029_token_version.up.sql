-- 000029: per-user token version.
--
-- Bumping token_version invalidates every inference token that user holds
-- (the claim carries the version it was minted at). The NOTIFY trigger
-- carries the bump into each pod's snapshot so /v1/* verification stays a
-- map read — users are otherwise outside the catalog.

ALTER TABLE users ADD COLUMN IF NOT EXISTS token_version INT NOT NULL DEFAULT 1;

-- The snapshot's whole view of a user is its token version, so an UPDATE of
-- anything else (last login, email, password hash) would rebuild it for
-- nothing. INSERT and DELETE always change the map.
DROP TRIGGER IF EXISTS users_notify ON users;
DROP TRIGGER IF EXISTS users_notify_write ON users;
DROP TRIGGER IF EXISTS users_notify_version ON users;
CREATE TRIGGER users_notify_write AFTER INSERT OR DELETE ON users
    FOR EACH ROW EXECUTE FUNCTION catalog_notify('user');
CREATE TRIGGER users_notify_version AFTER UPDATE ON users
    FOR EACH ROW WHEN (OLD.token_version IS DISTINCT FROM NEW.token_version)
    EXECUTE FUNCTION catalog_notify('user');
