-- 000035: accounts, pools, budgets; policy links to them; more policy-binding subject kinds.
--
-- Account is a provider org on one host; Pool is an ordered set of one host's keys; Budget is a recurring USD cap. host_id is a column so a host delete cascades, as for pricings and host_bindings.
--
-- A policy's links to pools, credentials, rate limits and budgets live in junctions like policy_host_keys. A deleted target drops the link rather than blocking the delete: the policy loses that cap or credential.
--
-- A HostKey's accountId lives in the secrets spec JSONB, beside its policyId.

CREATE TABLE IF NOT EXISTS accounts (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL UNIQUE,
    display_name     TEXT NOT NULL DEFAULT '',
    host_id          TEXT NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    metadata         JSONB NOT NULL,
    spec             JSONB NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resource_version BIGINT NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS accounts_host_idx ON accounts (host_id);

CREATE TABLE IF NOT EXISTS pools (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL UNIQUE,
    display_name     TEXT NOT NULL DEFAULT '',
    host_id          TEXT NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    metadata         JSONB NOT NULL,
    spec             JSONB NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resource_version BIGINT NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS pools_host_idx ON pools (host_id);

CREATE TABLE IF NOT EXISTS pool_credentials (
    pool_id     TEXT NOT NULL REFERENCES pools(id)   ON DELETE CASCADE,
    host_key_id TEXT NOT NULL REFERENCES secrets(id) ON DELETE CASCADE,
    position    INT  NOT NULL,
    PRIMARY KEY (pool_id, host_key_id)
);
CREATE INDEX IF NOT EXISTS pool_credentials_key_idx ON pool_credentials (host_key_id);

CREATE TABLE IF NOT EXISTS budgets (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL UNIQUE,
    display_name     TEXT NOT NULL DEFAULT '',
    metadata         JSONB NOT NULL,
    spec             JSONB NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resource_version BIGINT NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS policy_pools (
    policy_id TEXT NOT NULL REFERENCES policies(id) ON DELETE CASCADE,
    pool_id   TEXT NOT NULL REFERENCES pools(id)    ON DELETE CASCADE,
    position  INT  NOT NULL,
    PRIMARY KEY (policy_id, pool_id)
);
CREATE INDEX IF NOT EXISTS policy_pools_pool_idx ON policy_pools (pool_id);

CREATE TABLE IF NOT EXISTS policy_credentials (
    policy_id   TEXT NOT NULL REFERENCES policies(id) ON DELETE CASCADE,
    host_key_id TEXT NOT NULL REFERENCES secrets(id)  ON DELETE CASCADE,
    position    INT  NOT NULL,
    PRIMARY KEY (policy_id, host_key_id)
);
CREATE INDEX IF NOT EXISTS policy_credentials_key_idx ON policy_credentials (host_key_id);

-- One rate limit may appear more than once with different model scopes, so rows are keyed by position.
CREATE TABLE IF NOT EXISTS policy_rate_limits (
    policy_id     TEXT  NOT NULL REFERENCES policies(id)    ON DELETE CASCADE,
    rate_limit_id TEXT  NOT NULL REFERENCES rate_limits(id) ON DELETE CASCADE,
    models        JSONB NOT NULL DEFAULT '[]'::jsonb,
    position      INT   NOT NULL,
    PRIMARY KEY (policy_id, position)
);
CREATE INDEX IF NOT EXISTS policy_rate_limits_rl_idx ON policy_rate_limits (rate_limit_id);

CREATE TABLE IF NOT EXISTS policy_budgets (
    policy_id TEXT NOT NULL REFERENCES policies(id) ON DELETE CASCADE,
    budget_id TEXT NOT NULL REFERENCES budgets(id)  ON DELETE CASCADE,
    position  INT  NOT NULL,
    PRIMARY KEY (policy_id, budget_id)
);
CREATE INDEX IF NOT EXISTS policy_budgets_budget_idx ON policy_budgets (budget_id);

-- A policy-binding subject may also be a host, account, credential (host key), team or project. Each id-bearing kind fills its FK column so a deleted row drops out of every binding.
ALTER TABLE policy_binding_subjects DROP CONSTRAINT IF EXISTS policy_binding_subjects_kind_check;
ALTER TABLE policy_binding_subjects ADD CONSTRAINT policy_binding_subjects_kind_check
    CHECK (kind IN ('user', 'group', 'serviceaccount', 'host', 'account', 'credential', 'team', 'project'));
ALTER TABLE policy_binding_subjects
    ADD COLUMN IF NOT EXISTS subject_host_id       TEXT REFERENCES hosts(id)    ON DELETE CASCADE,
    ADD COLUMN IF NOT EXISTS subject_account_id    TEXT REFERENCES accounts(id) ON DELETE CASCADE,
    ADD COLUMN IF NOT EXISTS subject_credential_id TEXT REFERENCES secrets(id)  ON DELETE CASCADE,
    ADD COLUMN IF NOT EXISTS subject_team_id       TEXT REFERENCES teams(id)    ON DELETE CASCADE,
    ADD COLUMN IF NOT EXISTS subject_project_id    TEXT REFERENCES projects(id) ON DELETE CASCADE;
CREATE INDEX IF NOT EXISTS policy_binding_subjects_host_idx       ON policy_binding_subjects (subject_host_id);
CREATE INDEX IF NOT EXISTS policy_binding_subjects_account_idx    ON policy_binding_subjects (subject_account_id);
CREATE INDEX IF NOT EXISTS policy_binding_subjects_credential_idx ON policy_binding_subjects (subject_credential_id);
CREATE INDEX IF NOT EXISTS policy_binding_subjects_team_idx       ON policy_binding_subjects (subject_team_id);
CREATE INDEX IF NOT EXISTS policy_binding_subjects_project_idx    ON policy_binding_subjects (subject_project_id);

CREATE TRIGGER accounts_notify AFTER INSERT OR UPDATE OR DELETE ON accounts
    FOR EACH ROW EXECUTE FUNCTION catalog_notify('account');
CREATE TRIGGER pools_notify AFTER INSERT OR UPDATE OR DELETE ON pools
    FOR EACH ROW EXECUTE FUNCTION catalog_notify('pool');
CREATE TRIGGER budgets_notify AFTER INSERT OR UPDATE OR DELETE ON budgets
    FOR EACH ROW EXECUTE FUNCTION catalog_notify('budget');
CREATE TRIGGER pool_credentials_notify AFTER INSERT OR UPDATE OR DELETE ON pool_credentials
    FOR EACH ROW EXECUTE FUNCTION catalog_notify_junction('pool', 'pool_id');
CREATE TRIGGER policy_pools_notify AFTER INSERT OR UPDATE OR DELETE ON policy_pools
    FOR EACH ROW EXECUTE FUNCTION catalog_notify_junction('policy', 'policy_id');
CREATE TRIGGER policy_credentials_notify AFTER INSERT OR UPDATE OR DELETE ON policy_credentials
    FOR EACH ROW EXECUTE FUNCTION catalog_notify_junction('policy', 'policy_id');
CREATE TRIGGER policy_rate_limits_notify AFTER INSERT OR UPDATE OR DELETE ON policy_rate_limits
    FOR EACH ROW EXECUTE FUNCTION catalog_notify_junction('policy', 'policy_id');
CREATE TRIGGER policy_budgets_notify AFTER INSERT OR UPDATE OR DELETE ON policy_budgets
    FOR EACH ROW EXECUTE FUNCTION catalog_notify_junction('policy', 'policy_id');

CREATE TRIGGER accounts_resource_version BEFORE UPDATE ON accounts
    FOR EACH ROW EXECUTE FUNCTION bump_resource_version();
CREATE TRIGGER pools_resource_version BEFORE UPDATE ON pools
    FOR EACH ROW EXECUTE FUNCTION bump_resource_version();
CREATE TRIGGER budgets_resource_version BEFORE UPDATE ON budgets
    FOR EACH ROW EXECUTE FUNCTION bump_resource_version();
