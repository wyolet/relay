-- Reverse 000043: the triggers as 000011 and 000032 made them, then the column. The recorded times go with it.

DROP TRIGGER IF EXISTS relay_keys_notify_update ON relay_keys;
DROP TRIGGER IF EXISTS relay_keys_notify ON relay_keys;
CREATE TRIGGER relay_keys_notify  AFTER INSERT OR UPDATE OR DELETE ON relay_keys
    FOR EACH ROW EXECUTE FUNCTION catalog_notify('relaykey');

DROP TRIGGER IF EXISTS secrets_notify_update ON secrets;
DROP TRIGGER IF EXISTS secrets_notify ON secrets;
CREATE TRIGGER secrets_notify     AFTER INSERT OR UPDATE OR DELETE ON secrets
    FOR EACH ROW EXECUTE FUNCTION catalog_notify('hostkey');

DROP TRIGGER IF EXISTS relay_keys_resource_version ON relay_keys;
CREATE TRIGGER relay_keys_resource_version BEFORE UPDATE ON relay_keys
    FOR EACH ROW EXECUTE FUNCTION bump_resource_version();
DROP TRIGGER IF EXISTS secrets_resource_version ON secrets;
CREATE TRIGGER secrets_resource_version BEFORE UPDATE ON secrets
    FOR EACH ROW EXECUTE FUNCTION bump_resource_version();

ALTER TABLE relay_keys DROP COLUMN IF EXISTS last_used_at;
ALTER TABLE secrets    DROP COLUMN IF EXISTS last_used_at;
