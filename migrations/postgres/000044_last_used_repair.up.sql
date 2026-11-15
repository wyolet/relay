-- 000044: bring databases that ran the first 000043 (side tables relay_key_last_used / host_key_last_used) to the shape the current 000043 makes: a last_used_at column on the key rows, with notify and resource_version triggers that ignore it. Every statement is idempotent, so on a database the current 000043 already shaped this changes nothing.

DROP TABLE IF EXISTS relay_key_last_used;
DROP TABLE IF EXISTS host_key_last_used;

ALTER TABLE relay_keys ADD COLUMN IF NOT EXISTS last_used_at TIMESTAMPTZ;
ALTER TABLE secrets    ADD COLUMN IF NOT EXISTS last_used_at TIMESTAMPTZ;

DROP TRIGGER IF EXISTS relay_keys_resource_version ON relay_keys;
CREATE TRIGGER relay_keys_resource_version BEFORE UPDATE ON relay_keys
    FOR EACH ROW WHEN ((to_jsonb(OLD) - 'last_used_at' - 'resource_version')
                       IS DISTINCT FROM (to_jsonb(NEW) - 'last_used_at' - 'resource_version'))
    EXECUTE FUNCTION bump_resource_version();
DROP TRIGGER IF EXISTS secrets_resource_version ON secrets;
CREATE TRIGGER secrets_resource_version BEFORE UPDATE ON secrets
    FOR EACH ROW WHEN ((to_jsonb(OLD) - 'last_used_at' - 'resource_version')
                       IS DISTINCT FROM (to_jsonb(NEW) - 'last_used_at' - 'resource_version'))
    EXECUTE FUNCTION bump_resource_version();

DROP TRIGGER IF EXISTS relay_keys_notify ON relay_keys;
DROP TRIGGER IF EXISTS relay_keys_notify_update ON relay_keys;
CREATE TRIGGER relay_keys_notify AFTER INSERT OR DELETE ON relay_keys
    FOR EACH ROW EXECUTE FUNCTION catalog_notify('relaykey');
CREATE TRIGGER relay_keys_notify_update AFTER UPDATE ON relay_keys
    FOR EACH ROW WHEN ((to_jsonb(OLD) - 'last_used_at') IS DISTINCT FROM (to_jsonb(NEW) - 'last_used_at'))
    EXECUTE FUNCTION catalog_notify('relaykey');

DROP TRIGGER IF EXISTS secrets_notify ON secrets;
DROP TRIGGER IF EXISTS secrets_notify_update ON secrets;
CREATE TRIGGER secrets_notify AFTER INSERT OR DELETE ON secrets
    FOR EACH ROW EXECUTE FUNCTION catalog_notify('hostkey');
CREATE TRIGGER secrets_notify_update AFTER UPDATE ON secrets
    FOR EACH ROW WHEN ((to_jsonb(OLD) - 'last_used_at') IS DISTINCT FROM (to_jsonb(NEW) - 'last_used_at'))
    EXECUTE FUNCTION catalog_notify('hostkey');
