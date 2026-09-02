-- The generated `system` team / `legacy` project rows stay: they are data
-- an operator may have re-parented onto by now.
DROP TRIGGER IF EXISTS group_members_notify ON group_members;
DROP TRIGGER IF EXISTS groups_notify ON groups;
DROP TRIGGER IF EXISTS service_accounts_notify ON service_accounts;
DROP INDEX IF EXISTS relay_keys_previous_hash_idx;
ALTER TABLE relay_keys DROP CONSTRAINT IF EXISTS relay_keys_one_principal;
ALTER TABLE relay_keys
    DROP COLUMN IF EXISTS previous_key_hash,
    DROP COLUMN IF EXISTS principal_user_id,
    DROP COLUMN IF EXISTS principal_sa_id;
DROP TABLE IF EXISTS group_members;
DROP TABLE IF EXISTS groups;
DROP TABLE IF EXISTS service_accounts;
