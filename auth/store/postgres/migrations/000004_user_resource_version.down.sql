-- Drops what 000004 added; user writes go back to unconditional.
DROP TRIGGER IF EXISTS users_resource_version ON users;
DROP FUNCTION IF EXISTS bump_user_resource_version();
ALTER TABLE users DROP COLUMN IF EXISTS resource_version;
