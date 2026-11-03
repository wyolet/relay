DROP TRIGGER IF EXISTS users_notify_version ON users;
CREATE TRIGGER users_notify_version AFTER UPDATE ON users
    FOR EACH ROW WHEN (OLD.token_version IS DISTINCT FROM NEW.token_version
                       OR OLD.disabled IS DISTINCT FROM NEW.disabled)
    EXECUTE FUNCTION catalog_notify('user');

ALTER TABLE users DROP COLUMN IF EXISTS personal_account;
