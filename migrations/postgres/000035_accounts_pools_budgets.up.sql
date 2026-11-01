-- 000035: accounts, pools, budgets; policy links to them; more policy-binding subject kinds; team- and system-owned policy bindings.
--
-- Account is a provider org on one host; Pool is an ordered set of one host's keys; Budget is a recurring USD cap. host_id is a column so a host delete cascades, as for pricings and host_bindings.
--
-- A policy's links to pools, credentials, rate limits and budgets live in junctions like policy_host_keys. Deleting a linked rate limit or budget drops the link: the policy acts as if it never had that cap. Deleting a linked pool or credential keeps the link's id, which no longer resolves, so the policy narrows to what it still links instead of widening to ownership; those columns therefore carry no foreign key.
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
    pool_id   TEXT NOT NULL,
    position  INT  NOT NULL,
    PRIMARY KEY (policy_id, pool_id)
);
CREATE INDEX IF NOT EXISTS policy_pools_pool_idx ON policy_pools (pool_id);

CREATE TABLE IF NOT EXISTS policy_credentials (
    policy_id   TEXT NOT NULL REFERENCES policies(id) ON DELETE CASCADE,
    host_key_id TEXT NOT NULL,
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
-- A policy binding is owned by a project (project_id), a team (team_id) or the system (neither); deleting the owner deletes the binding.
ALTER TABLE policy_bindings ALTER COLUMN project_id DROP NOT NULL;
ALTER TABLE policy_bindings ADD COLUMN IF NOT EXISTS team_id TEXT REFERENCES teams(id) ON DELETE CASCADE;
ALTER TABLE policy_bindings DROP CONSTRAINT IF EXISTS policy_bindings_one_owner;
ALTER TABLE policy_bindings ADD CONSTRAINT policy_bindings_one_owner CHECK (project_id IS NULL OR team_id IS NULL);
CREATE INDEX IF NOT EXISTS policy_bindings_team_idx ON policy_bindings (team_id);
-- owner_scope copies the binding's owner onto each subject ('' system, 'team:<id>', 'project:<id>') so the one-binding rule can be per scope for users. Every binding before this release is project-owned.
ALTER TABLE policy_binding_subjects ADD COLUMN IF NOT EXISTS owner_scope TEXT NOT NULL DEFAULT '';
UPDATE policy_binding_subjects s SET owner_scope = 'project:' || b.project_id
FROM policy_bindings b WHERE b.id = s.binding_id AND b.project_id IS NOT NULL;
-- A subject is bound to at most one policy, across every binding; a user at most once per owner scope, since a binding applies only to requests inside its scope. Group subjects carry a name, not an id, and are not policy rungs. Subjects already bound twice stop the upgrade, listed, so the operator chooses which binding keeps them.
DO $$
DECLARE
    dups TEXT;
BEGIN
    SELECT string_agg(format('%s %s in %s', kind, subject_id, bindings), '; ' ORDER BY kind, subject_id) INTO dups
    FROM (
        SELECT s.kind, s.subject_id, string_agg(b.name, ', ' ORDER BY b.name) AS bindings
        FROM policy_binding_subjects s JOIN policy_bindings b ON b.id = s.binding_id
        WHERE s.subject_id IS NOT NULL
        GROUP BY s.kind, s.subject_id, CASE WHEN s.kind = 'user' THEN s.owner_scope END
        HAVING count(*) > 1
    ) d;
    IF dups IS NOT NULL THEN
        RAISE EXCEPTION 'migration 000035: a subject may be bound by one policy binding (a user by one per owner scope); remove the subject from all but one binding, then upgrade again: %', dups;
    END IF;
END $$;
CREATE UNIQUE INDEX IF NOT EXISTS policy_binding_subjects_one_binding ON policy_binding_subjects (kind, subject_id) WHERE kind <> 'user';
CREATE UNIQUE INDEX IF NOT EXISTS policy_binding_subjects_one_user_binding ON policy_binding_subjects (subject_id, owner_scope) WHERE kind = 'user';

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
