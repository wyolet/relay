-- Drops what 000002 added. users.oidc_subject still holds every account's primary identity; the others and every external id are lost.
ALTER TABLE groups DROP CONSTRAINT IF EXISTS groups_external_key, DROP COLUMN IF EXISTS external_id, DROP COLUMN IF EXISTS external_source;
ALTER TABLE users  DROP CONSTRAINT IF EXISTS users_external_key, DROP COLUMN IF EXISTS external_id, DROP COLUMN IF EXISTS external_source;
DROP TRIGGER IF EXISTS users_primary_identity ON users;
DROP FUNCTION IF EXISTS mirror_primary_identity();
DROP TABLE IF EXISTS identities;
