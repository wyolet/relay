-- Undoes 000007. The schema before it has no column for a user owner, so it refuses while a user owns a service account: delete those first.
--
-- A product that shares service_accounts (another table than role_binding_subjects references it, as 000001's down reads it) made the same change in its own migrations, so the column is left to the product's down migrations.

DO $$
DECLARE
    user_owned BIGINT;
BEGIN
    IF EXISTS (SELECT 1 FROM pg_constraint
                WHERE contype = 'f' AND confrelid = 'service_accounts'::regclass
                  AND conrelid::regclass::text <> 'role_binding_subjects') THEN
        RETURN;
    END IF;
    SELECT count(*) INTO user_owned FROM service_accounts WHERE user_id IS NOT NULL;
    IF user_owned > 0 THEN
        RAISE EXCEPTION 'auth migration 000007 down: % service accounts are owned by a user; delete them first', user_owned;
    END IF;
    ALTER TABLE service_accounts DROP CONSTRAINT IF EXISTS service_accounts_one_owner;
    DROP INDEX IF EXISTS service_accounts_user_idx;
    ALTER TABLE service_accounts DROP COLUMN IF EXISTS user_id;
    ALTER TABLE service_accounts ADD CONSTRAINT service_accounts_one_owner CHECK ((project_id IS NULL) <> (team_id IS NULL));
END
$$;
