-- Reverse 000034. Sealed rows cannot be read by the previous schema's binaries, so the down path refuses while any remain: run `relay secret downgrade` first, which rewrites them to the legacy shape under RELAY_MASTER_KEY.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM secret_values WHERE dek_id IS NOT NULL OR nonce IS NULL) THEN
        RAISE EXCEPTION 'secret_values holds values sealed under data keys; run `relay secret downgrade` before migrating below 34';
    END IF;
END;
$$;

DROP TRIGGER IF EXISTS secret_values_check ON secret_values;
DROP FUNCTION IF EXISTS secret_values_check();
DROP INDEX IF EXISTS secret_values_dek_id;
ALTER TABLE secret_values
    DROP COLUMN dek_id,
    DROP COLUMN scope,
    ALTER COLUMN nonce SET NOT NULL;

DROP TRIGGER IF EXISTS secret_keyring_notify ON secret_keyring;
DROP FUNCTION IF EXISTS secret_keyring_notify();
DROP TABLE IF EXISTS secret_keyring;
DROP TRIGGER IF EXISTS secret_data_keys_notify ON secret_data_keys;
DROP TABLE IF EXISTS secret_data_keys;
DROP TABLE IF EXISTS secret_values_legacy;
