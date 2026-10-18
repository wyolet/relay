-- 000034: stored secrets move to per-scope data keys.
--
-- secret_data_keys holds the data keys (DEKs), each wrapped by a key-encryption key; secret_keyring holds the target KEK new DEKs are wrapped under. secret_values rows gain the scope and the DEK that sealed them. Rows written before this migration keep the legacy shape (ciphertext + nonce under the master key, dek_id NULL) until relay rewrites them; secret_values_legacy is an untouched copy for the rollback runbook.

CREATE TABLE secret_values_legacy (LIKE secret_values INCLUDING ALL);
INSERT INTO secret_values_legacy SELECT * FROM secret_values;

CREATE TABLE secret_data_keys (
    id          UUID PRIMARY KEY,
    scope       TEXT        NOT NULL,
    state       TEXT        NOT NULL CHECK (state IN ('primary', 'decrypt', 'destroyed')),
    kek_id      BYTEA       NOT NULL,
    wrapped     BYTEA,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    demoted_at  TIMESTAMPTZ,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((state = 'destroyed') = (wrapped IS NULL))
);
CREATE UNIQUE INDEX secret_data_keys_one_primary ON secret_data_keys (scope) WHERE state = 'primary';

CREATE TABLE secret_keyring (
    singleton     BOOL PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    target_kek_id BYTEA
);
INSERT INTO secret_keyring DEFAULT VALUES;

CREATE TRIGGER secret_data_keys_notify AFTER INSERT OR UPDATE OR DELETE ON secret_data_keys
    FOR EACH ROW EXECUTE FUNCTION catalog_notify('secretkey');

CREATE OR REPLACE FUNCTION secret_keyring_notify() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('catalog_events', 'secretkey:upsert:target');
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER secret_keyring_notify AFTER INSERT OR UPDATE ON secret_keyring
    FOR EACH ROW EXECUTE FUNCTION secret_keyring_notify();

ALTER TABLE secret_values
    ADD COLUMN scope  TEXT,
    ADD COLUMN dek_id UUID REFERENCES secret_data_keys (id),
    ALTER COLUMN nonce DROP NOT NULL;
CREATE INDEX secret_values_dek_id ON secret_values (dek_id);

-- The database half of "writes are checked against key state": a sealed row must name the DEK in its header, and that DEK must be its scope's Primary (FOR SHARE serialises with a concurrent destroy or demotion). Legacy-bytes rows carry no DEK and are written only by `relay secret downgrade`, which sets relay.secret_legacy_write, so a binary from before this migration fails loudly instead of writing behind the rewriter. The SQLSTATEs are matched by relay's secret store.
CREATE OR REPLACE FUNCTION secret_values_check() RETURNS trigger AS $$
DECLARE
    key_state TEXT;
    key_scope TEXT;
BEGIN
    IF length(NEW.ciphertext) >= 5 AND substring(NEW.ciphertext FROM 1 FOR 4) = '\x0057534b'::bytea THEN
        IF NEW.dek_id IS NULL OR length(NEW.ciphertext) < 21
           OR NEW.dek_id <> encode(substring(NEW.ciphertext FROM 6 FOR 16), 'hex')::uuid THEN
            RAISE EXCEPTION 'secret_values %: dek_id does not match the sealed header', NEW.id USING ERRCODE = 'WSK02';
        END IF;
        SELECT state, scope INTO key_state, key_scope FROM secret_data_keys WHERE id = NEW.dek_id FOR SHARE;
        IF key_state IS DISTINCT FROM 'primary' OR key_scope IS DISTINCT FROM NEW.scope THEN
            RAISE EXCEPTION 'secret_values %: data key is not the primary of scope %', NEW.id, NEW.scope USING ERRCODE = 'WSK01';
        END IF;
    ELSE
        IF NEW.dek_id IS NOT NULL THEN
            RAISE EXCEPTION 'secret_values %: legacy-shape value with a dek_id', NEW.id USING ERRCODE = 'WSK02';
        END IF;
        IF current_setting('relay.secret_legacy_write', true) IS DISTINCT FROM 'on' THEN
            RAISE EXCEPTION 'secret_values %: legacy-shape writes are refused; this relay binary predates the data-key schema', NEW.id USING ERRCODE = 'WSK03';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER secret_values_check BEFORE INSERT OR UPDATE ON secret_values
    FOR EACH ROW EXECUTE FUNCTION secret_values_check();
