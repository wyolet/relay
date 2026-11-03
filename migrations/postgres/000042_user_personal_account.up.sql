-- 000042: a user's own say on their personal account.
--
-- NULL leaves it to the user's groups and the accounts settings section; 'on' or 'off' wins over both. Relay owns the column: the directory's user writes leave it alone. A change notifies like a disabled-flag change, so every pod's plans close or reopen the account.

ALTER TABLE users ADD COLUMN IF NOT EXISTS personal_account TEXT
    CONSTRAINT users_personal_account_check CHECK (personal_account IN ('on', 'off'));

DROP TRIGGER IF EXISTS users_notify_version ON users;
CREATE TRIGGER users_notify_version AFTER UPDATE ON users
    FOR EACH ROW WHEN (OLD.token_version IS DISTINCT FROM NEW.token_version
                       OR OLD.disabled IS DISTINCT FROM NEW.disabled
                       OR OLD.personal_account IS DISTINCT FROM NEW.personal_account)
    EXECUTE FUNCTION catalog_notify('user');
