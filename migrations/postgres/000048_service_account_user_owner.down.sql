-- Reverse 000048. The schema before it has no column for a user owner, so it refuses while a user owns a service account: delete those first.

DO $$
DECLARE
    user_owned BIGINT;
BEGIN
    SELECT count(*) INTO user_owned FROM service_accounts WHERE user_id IS NOT NULL;
    IF user_owned > 0 THEN
        RAISE EXCEPTION 'migration 000048 down: % service accounts are owned by a user; delete them first', user_owned;
    END IF;
END
$$;

ALTER TABLE service_accounts DROP CONSTRAINT IF EXISTS service_accounts_one_owner;
DROP INDEX IF EXISTS service_accounts_user_idx;
ALTER TABLE service_accounts DROP COLUMN IF EXISTS user_id;
ALTER TABLE service_accounts ADD CONSTRAINT service_accounts_one_owner CHECK ((project_id IS NULL) <> (team_id IS NULL));
