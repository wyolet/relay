-- 000040: provider accounts shared with another scope, and pools that draw on whole accounts.
--
-- A shared account is a provider account owned by the receiver that holds no keys of its own: source_account_id names the real account whose keys it reaches. share_receiver names who it was shared with ('user:<id>', 'team:<id>', 'project:<id>' or 'serviceaccount:<id>'; a service account's share is owned by its project and held by it), so one account is shared with one receiver once.
--
-- A share goes with its receiver: it fills team_id or project_id, holder_sa_id for a service account, or share_user_id for a user. The real account cannot be deleted while shares name it (the key takes no action, checked at the end of the statement): revoking them is an explicit step, and a share made while the real account is being deleted makes that delete fail instead of vanishing with it. A host delete takes both along in one statement.
--
-- pool_accounts lists the accounts a pool draws on, each contributing every key it holds; a deleted account drops out of every pool, as a deleted key does from pool_credentials.

ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS source_account_id TEXT REFERENCES accounts(id),
    ADD COLUMN IF NOT EXISTS share_receiver    TEXT,
    ADD COLUMN IF NOT EXISTS share_user_id     TEXT REFERENCES users(id) ON DELETE CASCADE;
ALTER TABLE accounts ADD CONSTRAINT accounts_share_shape
    CHECK ((source_account_id IS NULL) = (share_receiver IS NULL) AND (source_account_id IS NULL OR kind = 'provider')
           AND (share_user_id IS NULL OR source_account_id IS NOT NULL));
CREATE UNIQUE INDEX IF NOT EXISTS accounts_one_share ON accounts (source_account_id, share_receiver);
CREATE INDEX IF NOT EXISTS accounts_share_user_idx ON accounts (share_user_id);

CREATE TABLE IF NOT EXISTS pool_accounts (
    pool_id    TEXT NOT NULL REFERENCES pools(id)    ON DELETE CASCADE,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    position   INT  NOT NULL,
    PRIMARY KEY (pool_id, account_id)
);
CREATE INDEX IF NOT EXISTS pool_accounts_account_idx ON pool_accounts (account_id);
CREATE TRIGGER pool_accounts_notify AFTER INSERT OR UPDATE OR DELETE ON pool_accounts
    FOR EACH ROW EXECUTE FUNCTION catalog_notify_junction('pool', 'pool_id');
