DROP TRIGGER IF EXISTS users_notify ON users;
ALTER TABLE users DROP COLUMN IF EXISTS token_version;
