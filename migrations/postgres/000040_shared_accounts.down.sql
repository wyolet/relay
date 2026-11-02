-- Reverse 000040. Older releases cannot route through a shared account, so shared accounts are deleted, and with them the relay keys whose lane one was and the policy-binding entries holding one. Pools lose the accounts they listed and keep their keys.

DROP TABLE IF EXISTS pool_accounts;
DELETE FROM accounts WHERE source_account_id IS NOT NULL;
DROP INDEX IF EXISTS accounts_one_share;
ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_share_shape;
ALTER TABLE accounts
    DROP COLUMN IF EXISTS share_receiver,
    DROP COLUMN IF EXISTS source_account_id;
