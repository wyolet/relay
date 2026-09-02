-- The generated `system` team / `legacy` project rows stay: they are data
-- an operator may have re-parented onto by now.
--
-- The key rows the up migration rewrote are put back first: dropping the
-- columns would otherwise strand a user key with a project owner and a
-- spec.principal naming an account that is about to disappear, so a
-- re-applied up would re-parent it onto a generated service account.
UPDATE relay_keys
   SET metadata = jsonb_set(metadata, '{owner}',
                            jsonb_build_object('kind', 'user', 'id', principal_user_id), true)
 WHERE principal_user_id IS NOT NULL;
UPDATE relay_keys SET spec = spec - 'principal';

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
