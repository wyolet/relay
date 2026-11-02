-- 000039: accounts are what policies bind to and what usage is charged to.
--
-- An account is a provider org at a host (kind provider, as before), or one of the caller accounts relay keeps in step with the tenancy rows: one per team and per project, a personal account per user, one per service account, and a member account per user and team or project the user holds a role in. kind, the holder and the scope are columns so a deleted team, project, user or service account takes its accounts along; managed_key names a caller account's place ('team:<id>', 'personal:<user>', 'member:project:<id>:<user>', …) so concurrent writers converge on one row.
--
-- A policy binding binds accounts only; its subjects move to accounts below. A key lands in one of its principal's accounts, its default lane. A policy loses includeDeprecated, its directly linked credentials (each becomes a pool of one), its budget id list (now links with a shared flag) and its payload-logging flag (now on the accounts it governed).

ALTER TABLE accounts ALTER COLUMN host_id DROP NOT NULL;
ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS kind           TEXT    NOT NULL DEFAULT 'provider',
    ADD COLUMN IF NOT EXISTS managed_key    TEXT    UNIQUE,
    ADD COLUMN IF NOT EXISTS holder_user_id TEXT    REFERENCES users(id)            ON DELETE CASCADE,
    ADD COLUMN IF NOT EXISTS holder_sa_id   TEXT    REFERENCES service_accounts(id) ON DELETE CASCADE,
    ADD COLUMN IF NOT EXISTS team_id        TEXT    REFERENCES teams(id)            ON DELETE CASCADE,
    ADD COLUMN IF NOT EXISTS project_id     TEXT    REFERENCES projects(id)         ON DELETE CASCADE,
    ADD COLUMN IF NOT EXISTS closed         BOOLEAN NOT NULL DEFAULT FALSE,
    -- A member account created from a role binding closes when the last such grant goes; one created on first use (a group grant relay cannot see) stays open.
    ADD COLUMN IF NOT EXISTS follows_role_bindings BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE accounts ADD CONSTRAINT accounts_kind_check CHECK (kind IN ('team', 'project', 'personal', 'member', 'service', 'provider'));
ALTER TABLE accounts ADD CONSTRAINT accounts_provider_shape CHECK ((kind = 'provider') = (host_id IS NOT NULL) AND (kind = 'provider') = (managed_key IS NULL));
CREATE INDEX IF NOT EXISTS accounts_holder_user_idx ON accounts (holder_user_id);
CREATE INDEX IF NOT EXISTS accounts_holder_sa_idx   ON accounts (holder_sa_id);
CREATE INDEX IF NOT EXISTS accounts_team_idx        ON accounts (team_id);
CREATE INDEX IF NOT EXISTS accounts_project_idx     ON accounts (project_id);

-- The account name minted from base: slugified, then suffixed -2, -3, … while taken; the same rule as the write paths.
CREATE FUNCTION migration_000039_account_name(base TEXT) RETURNS TEXT AS $$
DECLARE
    s    TEXT;
    cand TEXT;
    n    INT := 2;
BEGIN
    s := regexp_replace(lower(base), '[^a-z0-9]+', '-', 'g');
    s := rtrim(left(btrim(s, '-'), 63), '-');
    IF s = '' THEN
        s := 'account';
    END IF;
    cand := s;
    WHILE EXISTS (SELECT 1 FROM accounts WHERE name = cand) LOOP
        cand := rtrim(left(s, 63 - length('-' || n)), '-') || '-' || n;
        n := n + 1;
    END LOOP;
    RETURN cand;
END $$ LANGUAGE plpgsql;

CREATE FUNCTION migration_000039_add_account(k TEXT, mkey TEXT, base TEXT, display TEXT, owner JSONB, h_user TEXT, h_sa TEXT, t_id TEXT, p_id TEXT, follows BOOLEAN) RETURNS TEXT AS $$
DECLARE
    acc TEXT;
BEGIN
    SELECT id INTO acc FROM accounts WHERE managed_key = mkey;
    IF acc IS NOT NULL THEN
        RETURN acc;
    END IF;
    acc := gen_random_uuid()::text;
    INSERT INTO accounts (id, name, display_name, host_id, metadata, spec, kind, managed_key, holder_user_id, holder_sa_id, team_id, project_id, follows_role_bindings)
    VALUES (acc, migration_000039_account_name(base), COALESCE(display, ''), NULL, jsonb_build_object('owner', owner), '{}'::jsonb, k, mkey, h_user, h_sa, t_id, p_id, follows);
    RETURN acc;
END $$ LANGUAGE plpgsql;

SELECT migration_000039_add_account('team', 'team:' || t.id, 'team-' || t.name, t.display_name,
       jsonb_build_object('kind', 'team', 'id', t.id), NULL, NULL, t.id, NULL, FALSE)
FROM teams t ORDER BY t.name;
SELECT migration_000039_add_account('project', 'project:' || p.id, 'project-' || p.name, p.display_name,
       jsonb_build_object('kind', 'project', 'id', p.id), NULL, NULL, NULL, p.id, FALSE)
FROM projects p ORDER BY p.name;
SELECT migration_000039_add_account('personal', 'personal:' || u.id, 'user-' || u.username, u.username,
       jsonb_build_object('kind', 'user', 'id', u.id), u.id, NULL, NULL, NULL, FALSE)
FROM users u ORDER BY u.username;
SELECT migration_000039_add_account('service', 'service:' || sa.id, 'sa-' || sa.name, sa.display_name,
       jsonb_build_object('kind', 'project', 'id', sa.project_id), NULL, sa.id, NULL, sa.project_id, FALSE)
FROM service_accounts sa ORDER BY sa.name;

-- A user named in an enabled role binding scoped to a team or project gets a member account there.
SELECT migration_000039_add_account('member', 'member:' || g.scope_kind || ':' || g.scope_id || ':' || u.id,
       u.username || '-' || g.scope_kind || '-' || g.scope_name, u.username || ' in ' || g.scope_display,
       jsonb_build_object('kind', g.scope_kind, 'id', g.scope_id), u.id, NULL,
       CASE WHEN g.scope_kind = 'team' THEN g.scope_id END, CASE WHEN g.scope_kind = 'project' THEN g.scope_id END, TRUE)
FROM (
    SELECT DISTINCT rb.scope_kind, rb.scope_id, s.subject_id AS user_id,
           COALESCE(t.name, p.name) AS scope_name, COALESCE(NULLIF(t.display_name, ''), t.name, NULLIF(p.display_name, ''), p.name) AS scope_display
    FROM role_bindings rb
    JOIN role_binding_subjects s ON s.binding_id = rb.id AND s.kind = 'user'
    LEFT JOIN teams t ON rb.scope_kind = 'team' AND t.id = rb.scope_id
    LEFT JOIN projects p ON rb.scope_kind = 'project' AND p.id = rb.scope_id
    WHERE rb.scope_kind IN ('team', 'project') AND COALESCE((rb.spec->>'enabled')::boolean, TRUE)
      AND (t.id IS NOT NULL OR p.id IS NOT NULL)
) g
JOIN users u ON u.id = g.user_id
ORDER BY u.username, g.scope_kind, g.scope_name;

-- A key's default lane: a service account's account, or the user's personal account.
ALTER TABLE relay_keys ADD COLUMN IF NOT EXISTS account_id TEXT REFERENCES accounts(id) ON DELETE CASCADE;
CREATE INDEX IF NOT EXISTS relay_keys_account_idx ON relay_keys (account_id);
UPDATE relay_keys k SET account_id = a.id
FROM accounts a
WHERE k.account_id IS NULL
  AND a.managed_key = CASE WHEN k.principal_sa_id IS NOT NULL THEN 'service:' || k.principal_sa_id ELSE 'personal:' || k.principal_user_id END;

-- Policy-binding subjects become accounts. A user under a team- or project-owned binding is that user's member account there (made when no role binding made it), any other user the personal account; a team, project or service account is its own account. Host and group subjects bind no account and are dropped, listed.
CREATE TABLE IF NOT EXISTS policy_binding_accounts (
    binding_id TEXT NOT NULL REFERENCES policy_bindings(id) ON DELETE CASCADE,
    account_id TEXT NOT NULL REFERENCES accounts(id)        ON DELETE CASCADE,
    position   INT  NOT NULL,
    PRIMARY KEY (binding_id, account_id)
);

DO $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN
        SELECT b.name AS binding, s.kind, COALESCE(h.name, s.subject_name, s.subject_id) AS subject
        FROM policy_binding_subjects s JOIN policy_bindings b ON b.id = s.binding_id
        LEFT JOIN hosts h ON s.kind = 'host' AND h.id = s.subject_id
        WHERE s.kind IN ('host', 'group') OR (s.kind = 'credential' AND COALESCE((SELECT spec->>'accountId' FROM secrets WHERE id = s.subject_id), '') = '')
        ORDER BY 1, s.position
    LOOP
        RAISE NOTICE '000039: dropping policy-binding subject: binding "%" -> % "%" (only accounts are bound)', r.binding, r.kind, r.subject;
    END LOOP;
END $$;

SELECT migration_000039_add_account('member', 'member:' || CASE WHEN b.project_id IS NOT NULL THEN 'project:' || b.project_id ELSE 'team:' || b.team_id END || ':' || u.id,
       u.username || '-' || CASE WHEN b.project_id IS NOT NULL THEN 'project-' || p.name ELSE 'team-' || t.name END,
       u.username || ' in ' || COALESCE(NULLIF(p.display_name, ''), p.name, NULLIF(t.display_name, ''), t.name),
       CASE WHEN b.project_id IS NOT NULL THEN jsonb_build_object('kind', 'project', 'id', b.project_id) ELSE jsonb_build_object('kind', 'team', 'id', b.team_id) END,
       u.id, NULL, b.team_id, b.project_id, FALSE)
FROM policy_binding_subjects s
JOIN policy_bindings b ON b.id = s.binding_id
JOIN users u ON u.id = s.subject_id
LEFT JOIN teams t ON t.id = b.team_id
LEFT JOIN projects p ON p.id = b.project_id
WHERE s.kind = 'user' AND (b.project_id IS NOT NULL OR b.team_id IS NOT NULL)
ORDER BY u.username;

CREATE TEMP TABLE binding_accounts AS
SELECT s.binding_id, s.position, b.name AS binding_name,
       CASE s.kind
           WHEN 'account'        THEN s.subject_id
           WHEN 'credential'     THEN (SELECT NULLIF(spec->>'accountId', '') FROM secrets WHERE id = s.subject_id)
           WHEN 'team'           THEN (SELECT id FROM accounts WHERE managed_key = 'team:' || s.subject_id)
           WHEN 'project'        THEN (SELECT id FROM accounts WHERE managed_key = 'project:' || s.subject_id)
           WHEN 'serviceaccount' THEN (SELECT id FROM accounts WHERE managed_key = 'service:' || s.subject_id)
           WHEN 'user'           THEN (SELECT id FROM accounts WHERE managed_key = CASE
                                          WHEN b.project_id IS NOT NULL THEN 'member:project:' || b.project_id || ':' || s.subject_id
                                          WHEN b.team_id IS NOT NULL THEN 'member:team:' || b.team_id || ':' || s.subject_id
                                          ELSE 'personal:' || s.subject_id END)
       END AS account_id
FROM policy_binding_subjects s JOIN policy_bindings b ON b.id = s.binding_id
WHERE s.kind NOT IN ('host', 'group');

-- An account is held by one binding. Two bindings reaching one account (a user bound by the system and by the user's own binding, which both become the personal account) stop the upgrade, listed, so the operator chooses which binding keeps it.
DO $$
DECLARE
    dups TEXT;
BEGIN
    SELECT string_agg(format('account %s in %s', a.name, d.bindings), '; ' ORDER BY a.name) INTO dups
    FROM (
        SELECT account_id, string_agg(DISTINCT binding_name, ', ') AS bindings
        FROM binding_accounts WHERE account_id IS NOT NULL
        GROUP BY account_id HAVING count(DISTINCT binding_id) > 1
    ) d JOIN accounts a ON a.id = d.account_id;
    IF dups IS NOT NULL THEN
        RAISE EXCEPTION 'migration 000039: policy-binding subjects become accounts, and an account is bound by one policy binding; remove the subject from all but one binding, then upgrade again: %', dups;
    END IF;
END $$;

INSERT INTO policy_binding_accounts (binding_id, account_id, position)
SELECT binding_id, account_id, (row_number() OVER (PARTITION BY binding_id ORDER BY position) - 1)::int
FROM (
    SELECT DISTINCT ON (binding_id, account_id) binding_id, account_id, position
    FROM binding_accounts WHERE account_id IS NOT NULL
    ORDER BY binding_id, account_id, position
) firsts;
DROP TABLE binding_accounts;

CREATE UNIQUE INDEX IF NOT EXISTS policy_binding_accounts_one_binding ON policy_binding_accounts (account_id);
CREATE TRIGGER policy_binding_accounts_notify AFTER INSERT OR UPDATE OR DELETE ON policy_binding_accounts
    FOR EACH ROW EXECUTE FUNCTION catalog_notify_junction('policybinding', 'binding_id');
DROP TABLE policy_binding_subjects;
UPDATE policy_bindings SET spec = spec - 'projectId' - 'subjects' WHERE spec ?| ARRAY['projectId', 'subjects'];

-- Policy models name a provider or a provider's model; hosts are the pools' business. An @host is stripped and the list deduplicated, each change listed. A bound policy whose list would end up empty (it named only hosts) would widen to every model, so it stops the upgrade, listed.
CREATE TEMP TABLE stripped_models AS
SELECT p.id, p.name,
       COALESCE((SELECT jsonb_agg(m ORDER BY first) FROM (
           SELECT split_part(e.value, '@', 1) AS m, min(e.n) AS first
           FROM jsonb_array_elements_text(CASE WHEN jsonb_typeof(p.models) = 'array' THEN p.models ELSE '[]'::jsonb END) WITH ORDINALITY AS e(value, n)
           WHERE split_part(e.value, '@', 1) <> ''
           GROUP BY 1
       ) kept), '[]'::jsonb) AS models,
       CASE WHEN jsonb_typeof(p.models) = 'array' THEN p.models ELSE '[]'::jsonb END AS was
FROM policies p;

DO $$
DECLARE
    widened TEXT;
    r       RECORD;
BEGIN
    SELECT string_agg(s.name, ', ' ORDER BY s.name) INTO widened
    FROM stripped_models s
    WHERE jsonb_array_length(s.was) > 0 AND jsonb_array_length(s.models) = 0
      AND EXISTS (SELECT 1 FROM policy_bindings b JOIN policy_binding_accounts a ON a.binding_id = b.id WHERE b.policy_id = s.id);
    IF widened IS NOT NULL THEN
        RAISE EXCEPTION 'migration 000039: these bound policies name only hosts in models, which would widen them to every model; name the providers or models they allow, then upgrade again: %', widened;
    END IF;
    FOR r IN SELECT name, was, models FROM stripped_models WHERE was <> models ORDER BY name LOOP
        RAISE NOTICE '000039: policy "%": models % become %', r.name, r.was, r.models;
    END LOOP;
END $$;
UPDATE policies p SET models = s.models FROM stripped_models s WHERE s.id = p.id AND s.was <> s.models;
DROP TABLE stripped_models;

UPDATE policy_rate_limits SET models = COALESCE((
    SELECT jsonb_agg(m ORDER BY first) FROM (
        SELECT split_part(e.value, '@', 1) AS m, min(e.n) AS first
        FROM jsonb_array_elements_text(models) WITH ORDINALITY AS e(value, n)
        WHERE split_part(e.value, '@', 1) <> ''
        GROUP BY 1
    ) kept), '[]'::jsonb)
WHERE EXISTS (SELECT 1 FROM jsonb_array_elements_text(models) e WHERE e LIKE '%@%');

-- A link counts once for the bound account (shared) or once per account under it; every link so far was shared.
ALTER TABLE policy_rate_limits ADD COLUMN IF NOT EXISTS shared BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE policy_budgets     ADD COLUMN IF NOT EXISTS shared BOOLEAN NOT NULL DEFAULT TRUE;

-- A directly linked credential becomes a pool of one: one pool per credential, owned like it, linked after the policy's pools in the order the credentials were tried.
CREATE TEMP TABLE credential_pools AS
SELECT DISTINCT ON (s.id) s.id AS host_key_id, gen_random_uuid()::text AS pool_id, s.name, s.display_name, s.spec->>'hostId' AS host_id, s.metadata->'owner' AS owner
FROM policy_credentials pc JOIN secrets s ON s.id = pc.host_key_id
JOIN hosts h ON h.id = s.spec->>'hostId'
ORDER BY s.id;

DO $$
DECLARE
    r    RECORD;
    cand TEXT;
    n    INT;
BEGIN
    FOR r IN SELECT * FROM credential_pools ORDER BY name LOOP
        cand := r.name;
        n := 2;
        WHILE EXISTS (SELECT 1 FROM pools WHERE name = cand) LOOP
            cand := rtrim(left(r.name, 63 - length('-' || n)), '-') || '-' || n;
            n := n + 1;
        END LOOP;
        INSERT INTO pools (id, name, display_name, host_id, metadata, spec)
        VALUES (r.pool_id, cand, r.display_name, r.host_id, jsonb_build_object('owner', COALESCE(r.owner, '{"kind":"system"}'::jsonb)), '{}'::jsonb);
        INSERT INTO pool_credentials (pool_id, host_key_id, position) VALUES (r.pool_id, r.host_key_id, 0);
        RAISE NOTICE '000039: host key "%" linked directly by a policy is now pool "%"', r.name, cand;
    END LOOP;
END $$;

INSERT INTO policy_pools (policy_id, pool_id, position)
SELECT pc.policy_id, cp.pool_id,
       (COALESCE((SELECT max(position) + 1 FROM policy_pools pp WHERE pp.policy_id = pc.policy_id), 0) + row_number() OVER (PARTITION BY pc.policy_id ORDER BY pc.position) - 1)::int
FROM policy_credentials pc JOIN credential_pools cp ON cp.host_key_id = pc.host_key_id
ON CONFLICT DO NOTHING;
DROP TABLE credential_pools;
DROP TABLE policy_credentials;

-- Payload logging moves from the policy to the accounts it governed: a policy that captured bodies turns capture on for each account bound to it.
UPDATE accounts a SET spec = jsonb_set(a.spec, '{payloadLogging}', '"on"')
FROM policy_binding_accounts pa JOIN policy_bindings b ON b.id = pa.binding_id JOIN policies p ON p.id = b.policy_id
WHERE pa.account_id = a.id AND COALESCE((p.spec->>'payloadLoggingEnabled')::boolean, FALSE);
UPDATE policies SET spec = spec - 'includeDeprecated' - 'payloadLoggingEnabled' - 'credentialIds' - 'budgetIds'
WHERE spec ?| ARRAY['includeDeprecated', 'payloadLoggingEnabled', 'credentialIds', 'budgetIds'];

DROP FUNCTION migration_000039_add_account(TEXT, TEXT, TEXT, TEXT, JSONB, TEXT, TEXT, TEXT, TEXT, BOOLEAN);
DROP FUNCTION migration_000039_account_name(TEXT);
