-- 000040: provider accounts shared with another scope, and pools that draw on whole accounts.
--
-- A shared account is a provider account owned by the receiver that holds no keys of its own: source_account_id names the real account whose keys it reaches. share_receiver names who it was shared with ('user:<id>', 'team:<id>', 'project:<id>' or 'serviceaccount:<id>'; a service account's share is owned by its project and held by it), so one account is shared with one receiver once. Deleting the real account takes its shares along.
--
-- pool_accounts lists the accounts a pool draws on, each contributing every key it holds; a deleted account drops out of every pool, as a deleted key does from pool_credentials.

ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS source_account_id TEXT REFERENCES accounts(id) ON DELETE CASCADE,
    ADD COLUMN IF NOT EXISTS share_receiver    TEXT;
ALTER TABLE accounts ADD CONSTRAINT accounts_share_shape
    CHECK ((source_account_id IS NULL) = (share_receiver IS NULL) AND (source_account_id IS NULL OR kind = 'provider'));
CREATE UNIQUE INDEX IF NOT EXISTS accounts_one_share ON accounts (source_account_id, share_receiver);

CREATE TABLE IF NOT EXISTS pool_accounts (
    pool_id    TEXT NOT NULL REFERENCES pools(id)    ON DELETE CASCADE,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    position   INT  NOT NULL,
    PRIMARY KEY (pool_id, account_id)
);
CREATE INDEX IF NOT EXISTS pool_accounts_account_idx ON pool_accounts (account_id);
CREATE TRIGGER pool_accounts_notify AFTER INSERT OR UPDATE OR DELETE ON pool_accounts
    FOR EACH ROW EXECUTE FUNCTION catalog_notify_junction('pool', 'pool_id');
