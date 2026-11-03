-- Reverse 000039. Bindings get their account subjects back, policies their budget ids and payload-logging flag (on when every account bound to them captured bodies). Caller accounts, keys' accountId and the shared flags are dropped; the pools made from directly linked credentials stay pools, and stripped @host suffixes stay stripped.

CREATE TABLE IF NOT EXISTS policy_binding_subjects (
    binding_id            TEXT NOT NULL REFERENCES policy_bindings(id)  ON DELETE CASCADE,
    kind                  TEXT NOT NULL,
    subject_id            TEXT,
    subject_name          TEXT,
    subject_user_id       TEXT REFERENCES users(id)            ON DELETE CASCADE,
    subject_sa_id         TEXT REFERENCES service_accounts(id) ON DELETE CASCADE,
    position              INT  NOT NULL,
    subject_host_id       TEXT REFERENCES hosts(id)    ON DELETE CASCADE,
    subject_account_id    TEXT REFERENCES accounts(id) ON DELETE CASCADE,
    subject_credential_id TEXT REFERENCES secrets(id)  ON DELETE CASCADE,
    subject_team_id       TEXT REFERENCES teams(id)    ON DELETE CASCADE,
    subject_project_id    TEXT REFERENCES projects(id) ON DELETE CASCADE,
    owner_scope           TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (binding_id, position),
    CHECK ((subject_id IS NULL) <> (subject_name IS NULL)),
    CONSTRAINT policy_binding_subjects_kind_check CHECK (kind IN ('user', 'group', 'serviceaccount', 'host', 'account', 'credential', 'team', 'project'))
);
CREATE INDEX IF NOT EXISTS policy_binding_subjects_user_idx       ON policy_binding_subjects (subject_user_id);
CREATE INDEX IF NOT EXISTS policy_binding_subjects_sa_idx         ON policy_binding_subjects (subject_sa_id);
CREATE INDEX IF NOT EXISTS policy_binding_subjects_name_idx       ON policy_binding_subjects (subject_name);
CREATE INDEX IF NOT EXISTS policy_binding_subjects_host_idx       ON policy_binding_subjects (subject_host_id);
CREATE INDEX IF NOT EXISTS policy_binding_subjects_account_idx    ON policy_binding_subjects (subject_account_id);
CREATE INDEX IF NOT EXISTS policy_binding_subjects_credential_idx ON policy_binding_subjects (subject_credential_id);
CREATE INDEX IF NOT EXISTS policy_binding_subjects_team_idx       ON policy_binding_subjects (subject_team_id);
CREATE INDEX IF NOT EXISTS policy_binding_subjects_project_idx    ON policy_binding_subjects (subject_project_id);

-- A caller account maps back to the subject it stands for; a provider account stays an account subject.
INSERT INTO policy_binding_subjects (binding_id, kind, subject_id, subject_user_id, subject_sa_id, subject_account_id, subject_team_id, subject_project_id, position, owner_scope)
SELECT pa.binding_id, m.kind, m.id,
       CASE WHEN m.kind = 'user' THEN m.id END, CASE WHEN m.kind = 'serviceaccount' THEN m.id END,
       CASE WHEN m.kind = 'account' THEN m.id END, CASE WHEN m.kind = 'team' THEN m.id END, CASE WHEN m.kind = 'project' THEN m.id END,
       pa.position,
       CASE WHEN b.project_id IS NOT NULL THEN 'project:' || b.project_id WHEN b.team_id IS NOT NULL THEN 'team:' || b.team_id WHEN b.user_id IS NOT NULL THEN 'user:' || b.user_id ELSE '' END
FROM policy_binding_accounts pa
JOIN policy_bindings b ON b.id = pa.binding_id
JOIN accounts a ON a.id = pa.account_id
CROSS JOIN LATERAL (SELECT CASE a.kind
        WHEN 'team' THEN 'team' WHEN 'project' THEN 'project' WHEN 'service' THEN 'serviceaccount'
        WHEN 'personal' THEN 'user' WHEN 'member' THEN 'user' ELSE 'account' END AS kind,
    CASE a.kind
        WHEN 'team' THEN a.team_id WHEN 'project' THEN a.project_id WHEN 'service' THEN a.holder_sa_id
        WHEN 'personal' THEN a.holder_user_id WHEN 'member' THEN a.holder_user_id ELSE a.id END AS id) m;

-- The older schema holds a user once per owner scope, and several accounts of one user (personal and member, or members fanned out from one binding) map back to that user. The oldest binding keeps the user; the rest are dropped, listed.
CREATE TEMP TABLE duplicate_users AS
SELECT binding_id, position, binding, subject_id, keeper FROM (
    SELECT s.binding_id, s.position, b.name AS binding, s.subject_id,
           first_value(b.name) OVER w AS keeper, row_number() OVER w AS n
    FROM policy_binding_subjects s JOIN policy_bindings b ON b.id = s.binding_id
    WHERE s.kind = 'user'
    WINDOW w AS (PARTITION BY s.subject_id, s.owner_scope ORDER BY b.created_at, b.id, s.position)
) ranked WHERE n > 1;
DO $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN SELECT * FROM duplicate_users ORDER BY binding, position LOOP
        IF r.binding = r.keeper THEN
            RAISE NOTICE '000039 down: binding "%" bound several accounts of user %; it binds the user once', r.binding, r.subject_id;
        ELSE
            RAISE NOTICE '000039 down: binding "%" no longer binds user % in the older schema; binding "%" keeps the user', r.binding, r.subject_id, r.keeper;
        END IF;
    END LOOP;
END $$;
DELETE FROM policy_binding_subjects s USING duplicate_users d WHERE s.binding_id = d.binding_id AND s.position = d.position;
DROP TABLE duplicate_users;

CREATE UNIQUE INDEX IF NOT EXISTS policy_binding_subjects_one_binding ON policy_binding_subjects (kind, subject_id) WHERE kind <> 'user';
CREATE UNIQUE INDEX IF NOT EXISTS policy_binding_subjects_one_user_binding ON policy_binding_subjects (subject_id, owner_scope) WHERE kind = 'user';
CREATE TRIGGER policy_binding_subjects_notify AFTER INSERT OR UPDATE OR DELETE ON policy_binding_subjects
    FOR EACH ROW EXECUTE FUNCTION catalog_notify_junction('policybinding', 'binding_id');

UPDATE policies p SET spec = jsonb_set(p.spec, '{payloadLoggingEnabled}', 'true')
WHERE EXISTS (SELECT 1 FROM policy_bindings b JOIN policy_binding_accounts pa ON pa.binding_id = b.id WHERE b.policy_id = p.id)
  AND NOT EXISTS (SELECT 1 FROM policy_bindings b JOIN policy_binding_accounts pa ON pa.binding_id = b.id JOIN accounts a ON a.id = pa.account_id
                  WHERE b.policy_id = p.id AND COALESCE(a.spec->>'payloadLogging', '') <> 'on');
DROP TABLE policy_binding_accounts;

CREATE TABLE IF NOT EXISTS policy_credentials (
    policy_id   TEXT NOT NULL REFERENCES policies(id) ON DELETE CASCADE,
    host_key_id TEXT NOT NULL,
    position    INT  NOT NULL,
    PRIMARY KEY (policy_id, host_key_id)
);
CREATE INDEX IF NOT EXISTS policy_credentials_key_idx ON policy_credentials (host_key_id);
CREATE TRIGGER policy_credentials_notify AFTER INSERT OR UPDATE OR DELETE ON policy_credentials
    FOR EACH ROW EXECUTE FUNCTION catalog_notify_junction('policy', 'policy_id');

-- 000035 keeps budget links in policy_budgets; only each-counted links have no older form, and they come back shared.
ALTER TABLE policy_budgets     DROP COLUMN IF EXISTS shared;
ALTER TABLE policy_rate_limits DROP COLUMN IF EXISTS shared;

DROP INDEX IF EXISTS relay_keys_account_idx;
ALTER TABLE relay_keys DROP COLUMN IF EXISTS account_id;

DELETE FROM accounts WHERE kind <> 'provider';
UPDATE accounts SET spec = spec - 'payloadLogging' WHERE spec ? 'payloadLogging';
DROP INDEX IF EXISTS accounts_holder_user_idx;
DROP INDEX IF EXISTS accounts_holder_sa_idx;
DROP INDEX IF EXISTS accounts_team_idx;
DROP INDEX IF EXISTS accounts_project_idx;
ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_provider_shape;
ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_kind_check;
ALTER TABLE accounts
    DROP COLUMN IF EXISTS follows_role_bindings,
    DROP COLUMN IF EXISTS closed,
    DROP COLUMN IF EXISTS project_id,
    DROP COLUMN IF EXISTS team_id,
    DROP COLUMN IF EXISTS holder_sa_id,
    DROP COLUMN IF EXISTS holder_user_id,
    DROP COLUMN IF EXISTS managed_key,
    DROP COLUMN IF EXISTS kind;
ALTER TABLE accounts ALTER COLUMN host_id SET NOT NULL;
