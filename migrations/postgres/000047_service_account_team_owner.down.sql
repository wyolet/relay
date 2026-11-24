-- Reverse 000047. The schema before it has no column for a team owner, so it refuses while a team owns a service account: delete those first.

DO $$
DECLARE
    team_owned BIGINT;
BEGIN
    SELECT count(*) INTO team_owned FROM service_accounts WHERE project_id IS NULL;
    IF team_owned > 0 THEN
        RAISE EXCEPTION 'migration 000047 down: % service accounts are owned by a team; delete them first', team_owned;
    END IF;
END
$$;

ALTER TABLE service_accounts DROP CONSTRAINT IF EXISTS service_accounts_one_owner;
DROP INDEX IF EXISTS service_accounts_team_idx;
ALTER TABLE service_accounts DROP COLUMN IF EXISTS team_id;
ALTER TABLE service_accounts ALTER COLUMN project_id SET NOT NULL;
