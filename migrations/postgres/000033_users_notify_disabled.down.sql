DROP TRIGGER IF EXISTS users_notify_version ON users;
CREATE TRIGGER users_notify_version AFTER UPDATE ON users
    FOR EACH ROW WHEN (OLD.token_version IS DISTINCT FROM NEW.token_version)
    EXECUTE FUNCTION catalog_notify('user');
