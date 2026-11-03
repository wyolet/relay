-- 000042: a user's own say on their personal account.
--
-- A row says 'on' or 'off' and wins over the user's groups and the accounts settings section; no row leaves it to them. The value sits in a table of relay's own, so the users table stays the auth module's shape. A change notifies as a change of the user, so every pod's plans close or reopen the account; the user's row version does not move, and the plans compare users by value.

CREATE TABLE IF NOT EXISTS user_personal_accounts (
    user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    value   TEXT NOT NULL CHECK (value IN ('on', 'off'))
);

CREATE TRIGGER user_personal_accounts_notify AFTER INSERT OR UPDATE OR DELETE ON user_personal_accounts
    FOR EACH ROW EXECUTE FUNCTION catalog_notify_junction('user', 'user_id');
