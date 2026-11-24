-- 000048: a user owns a service account directly, as their personal bot. The auth module's 000007 makes the same change to the table it shares.
--
-- A user's service accounts go with the user (ON DELETE CASCADE), as their personal account does, and take their keys and accounts along.

ALTER TABLE service_accounts ADD COLUMN IF NOT EXISTS user_id TEXT REFERENCES users(id) ON DELETE CASCADE;
ALTER TABLE service_accounts DROP CONSTRAINT IF EXISTS service_accounts_one_owner;
ALTER TABLE service_accounts ADD CONSTRAINT service_accounts_one_owner CHECK (num_nonnulls(project_id, team_id, user_id) = 1);
CREATE INDEX IF NOT EXISTS service_accounts_user_idx ON service_accounts (user_id);
