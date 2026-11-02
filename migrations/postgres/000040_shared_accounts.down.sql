-- Reverse 000040. Older releases cannot route through a shared account, so shared accounts are deleted: a relay key whose lane one was falls back to its principal's own account, and the policy-binding entries holding one go. Pools lose the accounts they listed and keep their keys.

DROP TABLE IF EXISTS pool_accounts;
UPDATE relay_keys SET account_id = NULL WHERE account_id IN (SELECT id FROM accounts WHERE source_account_id IS NOT NULL);
DELETE FROM accounts WHERE source_account_id IS NOT NULL;
DROP INDEX IF EXISTS accounts_share_user_idx;
DROP INDEX IF EXISTS accounts_one_share;
ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_share_shape;
ALTER TABLE accounts
    DROP COLUMN IF EXISTS share_user_id,
    DROP COLUMN IF EXISTS share_receiver,
    DROP COLUMN IF EXISTS source_account_id;
