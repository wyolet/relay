-- Reverse 000036. The tables and the columns come back empty: the links they held, and the spec fields 000036 removed, are not restored; every binding gets the default priority. Default accounts stay as plain accounts and keys keep the account they joined.

DROP INDEX IF EXISTS accounts_one_default;
ALTER TABLE accounts DROP COLUMN IF EXISTS default_owner;

ALTER TABLE policy_bindings ADD COLUMN IF NOT EXISTS priority INT NOT NULL DEFAULT 100;

ALTER TABLE policies ADD COLUMN IF NOT EXISTS rate_limit_id TEXT REFERENCES rate_limits(id) ON DELETE SET NULL;

CREATE TABLE IF NOT EXISTS policy_models (
    policy_id TEXT NOT NULL REFERENCES policies(id) ON DELETE CASCADE,
    model_id  TEXT NOT NULL REFERENCES models(id)   ON DELETE RESTRICT,
    position  INT  NOT NULL,
    PRIMARY KEY (policy_id, model_id)
);
CREATE INDEX IF NOT EXISTS policy_models_model_idx ON policy_models (model_id);

CREATE TABLE IF NOT EXISTS policy_host_keys (
    policy_id   TEXT NOT NULL REFERENCES policies(id) ON DELETE CASCADE,
    host_key_id TEXT NOT NULL REFERENCES secrets(id)  ON DELETE RESTRICT,
    position    INT  NOT NULL,
    PRIMARY KEY (policy_id, host_key_id)
);
CREATE INDEX IF NOT EXISTS policy_host_keys_key_idx ON policy_host_keys (host_key_id);

CREATE TRIGGER policy_models_notify AFTER INSERT OR UPDATE OR DELETE ON policy_models
    FOR EACH ROW EXECUTE FUNCTION catalog_notify_junction('policy', 'policy_id');
CREATE TRIGGER policy_host_keys_notify AFTER INSERT OR UPDATE OR DELETE ON policy_host_keys
    FOR EACH ROW EXECUTE FUNCTION catalog_notify_junction('policy', 'policy_id');
