-- Undoes 000006. The schema before it has no column for a team owner, so it refuses while a team owns a service account: delete those first.
--
-- A product that shares service_accounts (another table than role_binding_subjects references it, as 000001's down reads it) made the same change in its own migrations, so the columns are left to the product's down migrations.

DO $$
DECLARE
    team_owned BIGINT;
BEGIN
    IF EXISTS (SELECT 1 FROM pg_constraint
                WHERE contype = 'f' AND confrelid = 'service_accounts'::regclass
                  AND conrelid::regclass::text <> 'role_binding_subjects') THEN
        RETURN;
    END IF;
    -- Holds off a service account written while the rows are counted; the ALTERs below take it anyway.
    LOCK TABLE service_accounts IN ACCESS EXCLUSIVE MODE;
    SELECT count(*) INTO team_owned FROM service_accounts WHERE project_id IS NULL;
    IF team_owned > 0 THEN
        RAISE EXCEPTION 'auth migration 000006 down: % service accounts are owned by a team; delete them first', team_owned;
    END IF;
    ALTER TABLE service_accounts DROP CONSTRAINT IF EXISTS service_accounts_one_owner;
    DROP INDEX IF EXISTS service_accounts_team_idx;
    ALTER TABLE service_accounts DROP COLUMN IF EXISTS team_id;
    ALTER TABLE service_accounts ALTER COLUMN project_id SET NOT NULL;
END
$$;
