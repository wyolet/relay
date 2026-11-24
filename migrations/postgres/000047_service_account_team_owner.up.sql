-- 000047: a service account is owned by a project or, with no project in between, by a team. The auth module's 000006 makes the same change to the table it shares.
--
-- A team's own service accounts block its delete (ON DELETE RESTRICT) instead of going with it: their keys and accounts are in use until someone removes them.

ALTER TABLE service_accounts ALTER COLUMN project_id DROP NOT NULL;
ALTER TABLE service_accounts ADD COLUMN IF NOT EXISTS team_id TEXT REFERENCES teams(id) ON DELETE RESTRICT;
ALTER TABLE service_accounts DROP CONSTRAINT IF EXISTS service_accounts_one_owner;
ALTER TABLE service_accounts ADD CONSTRAINT service_accounts_one_owner CHECK ((project_id IS NULL) <> (team_id IS NULL));
CREATE INDEX IF NOT EXISTS service_accounts_team_idx ON service_accounts (team_id);
