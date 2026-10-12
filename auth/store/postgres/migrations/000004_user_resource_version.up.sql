-- users gets the resource version the other directory tables carry, so a user write applies only while the row is as its writer read it. The trigger has a function of its own: 000001's bump_resource_version may be a product's, dropped by that product's down migrations.
ALTER TABLE users ADD COLUMN IF NOT EXISTS resource_version BIGINT NOT NULL DEFAULT 1;

CREATE OR REPLACE FUNCTION bump_user_resource_version() RETURNS trigger AS $fn$
BEGIN
    NEW.resource_version := OLD.resource_version + 1;
    RETURN NEW;
END;
$fn$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS users_resource_version ON users;
CREATE TRIGGER users_resource_version BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION bump_user_resource_version();
