-- 000043: when each relay key and host key last served a request, to the minute.
--
-- A runtime-only column on the row, written by every pod about once a minute. An UPDATE that changes only last_used_at neither notifies catalog_events (each pod would rebuild its snapshot) nor bumps resource_version (an operator's edit would read as stale); every other change still does both. No index covers the column, so those writes stay HOT-eligible.
--
-- A WHEN clause cannot read OLD on INSERT or NEW on DELETE, so each *_notify trigger keeps INSERT and DELETE and its UPDATE moves to *_notify_update.

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
