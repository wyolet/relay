-- 000029: per-user token version.
--
-- Bumping token_version invalidates every inference token that user holds
-- (the claim carries the version it was minted at). The NOTIFY trigger
-- carries the bump into each pod's snapshot so /v1/* verification stays a
-- map read — users are otherwise outside the catalog.

ALTER TABLE users ADD COLUMN IF NOT EXISTS token_version INT NOT NULL DEFAULT 1;

DROP TRIGGER IF EXISTS users_notify ON users;
CREATE TRIGGER users_notify AFTER INSERT OR UPDATE OR DELETE ON users
    FOR EACH ROW EXECUTE FUNCTION catalog_notify('user');
