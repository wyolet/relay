-- 000033: the users UPDATE notify also fires on a disabled-flag change.
--
-- The snapshot carries the disabled ids as well as the token versions. Every store write that flips the flag bumps token_version too, but an UPDATE outside the stores would leave every pod's snapshot treating the account as enabled.

DROP TRIGGER IF EXISTS users_notify_version ON users;
CREATE TRIGGER users_notify_version AFTER UPDATE ON users
    FOR EACH ROW WHEN (OLD.token_version IS DISTINCT FROM NEW.token_version
                       OR OLD.disabled IS DISTINCT FROM NEW.disabled)
    EXECUTE FUNCTION catalog_notify('user');
