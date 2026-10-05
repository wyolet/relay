-- 000032: per-row resource version for optimistic concurrency.
--
-- A control-plane update may carry the version it read; the store's upsert
-- only applies when the stored version still matches. The trigger bumps the
-- version on every UPDATE, so each write path (CRUD, apply, detach, seed,
-- status and rotation writes) changes it without having to remember to.

ALTER TABLE providers        ADD COLUMN IF NOT EXISTS resource_version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE hosts            ADD COLUMN IF NOT EXISTS resource_version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE models           ADD COLUMN IF NOT EXISTS resource_version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE secrets          ADD COLUMN IF NOT EXISTS resource_version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE rate_limits      ADD COLUMN IF NOT EXISTS resource_version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE policies         ADD COLUMN IF NOT EXISTS resource_version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE pricings         ADD COLUMN IF NOT EXISTS resource_version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE host_bindings    ADD COLUMN IF NOT EXISTS resource_version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE relay_keys       ADD COLUMN IF NOT EXISTS resource_version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE teams            ADD COLUMN IF NOT EXISTS resource_version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE projects         ADD COLUMN IF NOT EXISTS resource_version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE service_accounts ADD COLUMN IF NOT EXISTS resource_version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE groups           ADD COLUMN IF NOT EXISTS resource_version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE roles            ADD COLUMN IF NOT EXISTS resource_version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE role_bindings    ADD COLUMN IF NOT EXISTS resource_version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE policy_bindings  ADD COLUMN IF NOT EXISTS resource_version BIGINT NOT NULL DEFAULT 1;

CREATE OR REPLACE FUNCTION bump_resource_version() RETURNS trigger AS $$
BEGIN
    NEW.resource_version := OLD.resource_version + 1;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS providers_resource_version ON providers;
CREATE TRIGGER providers_resource_version BEFORE UPDATE ON providers
    FOR EACH ROW EXECUTE FUNCTION bump_resource_version();
DROP TRIGGER IF EXISTS hosts_resource_version ON hosts;
CREATE TRIGGER hosts_resource_version BEFORE UPDATE ON hosts
    FOR EACH ROW EXECUTE FUNCTION bump_resource_version();
DROP TRIGGER IF EXISTS models_resource_version ON models;
CREATE TRIGGER models_resource_version BEFORE UPDATE ON models
    FOR EACH ROW EXECUTE FUNCTION bump_resource_version();
DROP TRIGGER IF EXISTS secrets_resource_version ON secrets;
CREATE TRIGGER secrets_resource_version BEFORE UPDATE ON secrets
    FOR EACH ROW EXECUTE FUNCTION bump_resource_version();
DROP TRIGGER IF EXISTS rate_limits_resource_version ON rate_limits;
CREATE TRIGGER rate_limits_resource_version BEFORE UPDATE ON rate_limits
    FOR EACH ROW EXECUTE FUNCTION bump_resource_version();
DROP TRIGGER IF EXISTS policies_resource_version ON policies;
CREATE TRIGGER policies_resource_version BEFORE UPDATE ON policies
    FOR EACH ROW EXECUTE FUNCTION bump_resource_version();
DROP TRIGGER IF EXISTS pricings_resource_version ON pricings;
CREATE TRIGGER pricings_resource_version BEFORE UPDATE ON pricings
    FOR EACH ROW EXECUTE FUNCTION bump_resource_version();
DROP TRIGGER IF EXISTS host_bindings_resource_version ON host_bindings;
CREATE TRIGGER host_bindings_resource_version BEFORE UPDATE ON host_bindings
    FOR EACH ROW EXECUTE FUNCTION bump_resource_version();
DROP TRIGGER IF EXISTS relay_keys_resource_version ON relay_keys;
CREATE TRIGGER relay_keys_resource_version BEFORE UPDATE ON relay_keys
    FOR EACH ROW EXECUTE FUNCTION bump_resource_version();
DROP TRIGGER IF EXISTS teams_resource_version ON teams;
CREATE TRIGGER teams_resource_version BEFORE UPDATE ON teams
    FOR EACH ROW EXECUTE FUNCTION bump_resource_version();
DROP TRIGGER IF EXISTS projects_resource_version ON projects;
CREATE TRIGGER projects_resource_version BEFORE UPDATE ON projects
    FOR EACH ROW EXECUTE FUNCTION bump_resource_version();
DROP TRIGGER IF EXISTS service_accounts_resource_version ON service_accounts;
CREATE TRIGGER service_accounts_resource_version BEFORE UPDATE ON service_accounts
    FOR EACH ROW EXECUTE FUNCTION bump_resource_version();
DROP TRIGGER IF EXISTS groups_resource_version ON groups;
CREATE TRIGGER groups_resource_version BEFORE UPDATE ON groups
    FOR EACH ROW EXECUTE FUNCTION bump_resource_version();
DROP TRIGGER IF EXISTS roles_resource_version ON roles;
CREATE TRIGGER roles_resource_version BEFORE UPDATE ON roles
    FOR EACH ROW EXECUTE FUNCTION bump_resource_version();
DROP TRIGGER IF EXISTS role_bindings_resource_version ON role_bindings;
CREATE TRIGGER role_bindings_resource_version BEFORE UPDATE ON role_bindings
    FOR EACH ROW EXECUTE FUNCTION bump_resource_version();
DROP TRIGGER IF EXISTS policy_bindings_resource_version ON policy_bindings;
CREATE TRIGGER policy_bindings_resource_version BEFORE UPDATE ON policy_bindings
    FOR EACH ROW EXECUTE FUNCTION bump_resource_version();
