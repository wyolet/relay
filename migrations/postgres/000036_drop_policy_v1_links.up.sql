-- 000036: drop the pre-v0.20 policy links. A policy now reaches keys through its pools and credentials and is capped by its rate limits and budgets (000035); a host key names its account; keys and service accounts hold no policy, a policy binding binds their principal. A subject has one binding, so bindings carry no priority; a host's tier policies are bound to it, so it lists no policy menu. A team's or project's spend cap is a Budget its policy links, so they carry no budget of their own.
--
-- Moving data to the new shape is manual, so nothing is converted: the legacy model-id and host-key junctions, the single rate-limit column, the binding priority, and the old fields inside the spec documents are removed.

-- A policy bound to a subject keeps governing it after the upgrade, and an empty list does not narrow, so dropping a bound policy's only model-id or host-key list would widen what its subjects reach. Such policies stop the upgrade, listed, so the operator unbinds them or clears that list knowingly first. Group subjects bind nothing after the upgrade and do not count.
DO $$
DECLARE
    widened TEXT;
BEGIN
    SELECT string_agg(format('policy %s loses its %s (bound by %s)', p.name, l.lost, l.bindings), '; ' ORDER BY p.name) INTO widened
    FROM policies p
    CROSS JOIN LATERAL (
        SELECT concat_ws(' and ',
                   CASE WHEN EXISTS (SELECT 1 FROM policy_models pm WHERE pm.policy_id = p.id)
                         AND (CASE WHEN jsonb_typeof(p.models) = 'array' THEN jsonb_array_length(p.models) ELSE 0 END) = 0
                        THEN 'model ids' END,
                   CASE WHEN EXISTS (SELECT 1 FROM policy_host_keys phk WHERE phk.policy_id = p.id)
                         AND NOT EXISTS (SELECT 1 FROM policy_pools pp WHERE pp.policy_id = p.id)
                         AND NOT EXISTS (SELECT 1 FROM policy_credentials pc WHERE pc.policy_id = p.id)
                        THEN 'host keys' END) AS lost,
               (SELECT string_agg(DISTINCT b.name, ', ' ORDER BY b.name)
                FROM policy_bindings b JOIN policy_binding_subjects s ON s.binding_id = b.id
                WHERE b.policy_id = p.id AND s.subject_id IS NOT NULL) AS bindings
    ) l
    WHERE l.lost <> '' AND l.bindings IS NOT NULL;
    IF widened IS NOT NULL THEN
        RAISE EXCEPTION 'migration 000036: bound policies would lose their only model or host-key restriction and widen; remove them from their bindings, or delete their policy_models / policy_host_keys rows if the wider reach is intended, then upgrade again and re-create the restriction as models, pools or credentials: %', widened;
    END IF;
END $$;

-- Each link removed below, and each binding that stops governing what it governed, is reported by name first, so the operator running the upgrade can re-create the ones still wanted. A target whose row is gone is reported by id.
DO $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN
        SELECT k.name AS src, COALESCE(p.name, k.spec->>'policyId') AS dst
        FROM relay_keys k LEFT JOIN policies p ON p.id = k.spec->>'policyId'
        WHERE COALESCE(k.spec->>'policyId', '') <> ''
        ORDER BY 1, 2
    LOOP
        RAISE NOTICE '000036: dropping link: key "%" -> policy "%"', r.src, r.dst;
    END LOOP;
    FOR r IN
        SELECT s.name AS src, COALESCE(p.name, s.spec->>'policyId') AS dst
        FROM secrets s LEFT JOIN policies p ON p.id = s.spec->>'policyId'
        WHERE COALESCE(s.spec->>'policyId', '') <> ''
        ORDER BY 1, 2
    LOOP
        RAISE NOTICE '000036: dropping link: host key "%" -> tier policy "%"', r.src, r.dst;
    END LOOP;
    FOR r IN
        SELECT sa.name AS src, COALESCE(p.name, sa.spec->>'policyId') AS dst
        FROM service_accounts sa LEFT JOIN policies p ON p.id = sa.spec->>'policyId'
        WHERE COALESCE(sa.spec->>'policyId', '') <> ''
        ORDER BY 1, 2
    LOOP
        RAISE NOTICE '000036: dropping link: service account "%" -> policy "%"', r.src, r.dst;
    END LOOP;
    -- Bindings stay, but a group subject (system:authenticated included) binds nothing now, and a project's binding on a user governs only that user's tokens in the project, no longer the user's personal keys.
    FOR r IN
        SELECT b.name AS src, p.name AS pol, s.subject_name AS grp
        FROM policy_binding_subjects s JOIN policy_bindings b ON b.id = s.binding_id JOIN policies p ON p.id = b.policy_id
        WHERE s.kind = 'group'
        ORDER BY 1, s.position
    LOOP
        RAISE NOTICE '000036: binding no longer applies: policy binding "%" (policy "%") -> group "%"', r.src, r.pol, r.grp;
    END LOOP;
    FOR r IN
        SELECT b.name AS src, p.name AS pol, COALESCE(u.username, s.subject_id) AS usr, pr.name AS proj
        FROM policy_binding_subjects s JOIN policy_bindings b ON b.id = s.binding_id JOIN policies p ON p.id = b.policy_id
        JOIN projects pr ON pr.id = b.project_id LEFT JOIN users u ON u.id = s.subject_id
        WHERE s.kind = 'user'
        ORDER BY 1, s.position
    LOOP
        RAISE NOTICE '000036: binding narrows to project tokens: policy binding "%" (policy "%") -> user "%" governs only tokens in project "%", not personal keys', r.src, r.pol, r.usr, r.proj;
    END LOOP;
    FOR r IN
        SELECT p.name AS src, COALESCE(s.name, phk.host_key_id) AS dst
        FROM policy_host_keys phk JOIN policies p ON p.id = phk.policy_id LEFT JOIN secrets s ON s.id = phk.host_key_id
        ORDER BY 1, phk.position
    LOOP
        RAISE NOTICE '000036: dropping link: policy "%" -> host key "%"', r.src, r.dst;
    END LOOP;
    FOR r IN
        SELECT p.name AS src, COALESCE(m.name, pm.model_id) AS dst
        FROM policy_models pm JOIN policies p ON p.id = pm.policy_id LEFT JOIN models m ON m.id = pm.model_id
        ORDER BY 1, pm.position
    LOOP
        RAISE NOTICE '000036: dropping link: policy "%" -> model "%"', r.src, r.dst;
    END LOOP;
    FOR r IN
        SELECT p.name AS src, COALESCE(rl.name, p.rate_limit_id) AS dst
        FROM policies p LEFT JOIN rate_limits rl ON rl.id = p.rate_limit_id
        WHERE p.rate_limit_id IS NOT NULL
        ORDER BY 1
    LOOP
        RAISE NOTICE '000036: dropping link: policy "%" -> rate limit "%"', r.src, r.dst;
    END LOOP;
    FOR r IN
        SELECT p.name AS src, COALESCE(rl.name, b.value->>'rateLimitId') AS dst,
               COALESCE((SELECT string_agg(m, ', ') FROM jsonb_array_elements_text(COALESCE(b.value->'models', '[]'::jsonb)) AS m), '') AS models
        FROM policies p
        CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(p.spec->'rlBindings') = 'array' THEN p.spec->'rlBindings' ELSE '[]'::jsonb END) WITH ORDINALITY AS b(value, n)
        LEFT JOIN rate_limits rl ON rl.id = b.value->>'rateLimitId'
        ORDER BY 1, b.n
    LOOP
        RAISE NOTICE '000036: dropping link: policy "%" -> rate limit "%" for models %', r.src, r.dst, r.models;
    END LOOP;
END $$;

DROP TABLE IF EXISTS policy_models;
DROP TABLE IF EXISTS policy_host_keys;
ALTER TABLE policies DROP COLUMN IF EXISTS rate_limit_id;
ALTER TABLE policy_bindings DROP COLUMN IF EXISTS priority;
UPDATE hosts SET spec = spec - 'policies' - 'defaultPolicy' WHERE spec ?| ARRAY['policies', 'defaultPolicy'];

UPDATE policies SET spec = spec - 'modelIds' - 'hostKeyIds' - 'rateLimitId' - 'rlBindings' - 'keySelection'
WHERE spec ?| ARRAY['modelIds', 'hostKeyIds', 'rateLimitId', 'rlBindings', 'keySelection'];
UPDATE relay_keys SET spec = spec - 'policyId' WHERE spec ? 'policyId';
UPDATE service_accounts SET spec = spec - 'policyId' WHERE spec ? 'policyId';
UPDATE secrets SET spec = spec - 'policyId' WHERE spec ? 'policyId';
UPDATE teams SET spec = spec - 'budget' WHERE spec ? 'budget';
UPDATE projects SET spec = spec - 'budget' WHERE spec ? 'budget';

-- A host key saved without an account joins its owner's default account at its host, created on first use. default_owner marks that account with its owner ('system' or '<kind>:<id>'), so each owner has one per host; other accounts leave it NULL.
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS default_owner TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS accounts_one_default ON accounts (host_id, default_owner);

-- A default account no key names goes with its team or project in the same transaction, as it goes with its host through host_id; one a key still names stays, and the delete paths refuse while it does.
CREATE OR REPLACE FUNCTION accounts_drop_keyless_defaults() RETURNS trigger AS $$
BEGIN
    DELETE FROM accounts a
    WHERE a.default_owner = TG_ARGV[0] || ':' || OLD.id
      AND NOT EXISTS (SELECT 1 FROM secrets s WHERE s.spec->>'accountId' = a.id);
    RETURN NULL;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER teams_drop_keyless_default_accounts AFTER DELETE ON teams
    FOR EACH ROW EXECUTE FUNCTION accounts_drop_keyless_defaults('team');
CREATE TRIGGER projects_drop_keyless_default_accounts AFTER DELETE ON projects
    FOR EACH ROW EXECUTE FUNCTION accounts_drop_keyless_defaults('project');

-- Existing keys without an account join their owner's default account the same way. Its name is the host's, suffixed -2, -3, … when taken. An owner without a kind, or a user without an id, is the system, as the write paths stamp it. Keys whose host is gone stay as they are.
DO $$
DECLARE
    r    RECORD;
    acc  TEXT;
    cand TEXT;
    n    INT;
BEGIN
    FOR r IN
        SELECT k.id, k.kind, k.owner_id,
               CASE WHEN k.kind = 'system' THEN 'system' ELSE k.kind || ':' || k.owner_id END AS scope,
               h.id AS host_id, h.name AS host_name, h.display_name AS host_display
        FROM (
            SELECT s.id, s.spec->>'hostId' AS host_id,
                   CASE WHEN COALESCE(s.metadata->'owner'->>'kind', '') IN ('', 'system')
                          OR (s.metadata->'owner'->>'kind' = 'user' AND COALESCE(s.metadata->'owner'->>'id', '') = '')
                        THEN 'system' ELSE s.metadata->'owner'->>'kind' END AS kind,
                   COALESCE(s.metadata->'owner'->>'id', '') AS owner_id
            FROM secrets s
            WHERE COALESCE(s.spec->>'accountId', '') = ''
        ) k
        JOIN hosts h ON h.id = k.host_id
        ORDER BY k.id
    LOOP
        SELECT id INTO acc FROM accounts WHERE host_id = r.host_id AND default_owner = r.scope;
        IF acc IS NULL THEN
            cand := r.host_name;
            n := 2;
            WHILE EXISTS (SELECT 1 FROM accounts WHERE name = cand) LOOP
                cand := rtrim(left(r.host_name, 63 - length('-' || n)), '-') || '-' || n;
                n := n + 1;
            END LOOP;
            acc := gen_random_uuid()::text;
            INSERT INTO accounts (id, name, display_name, host_id, metadata, spec, default_owner)
            VALUES (acc, cand, r.host_display, r.host_id,
                    jsonb_build_object('owner', CASE WHEN r.scope = 'system' THEN jsonb_build_object('kind', 'system')
                                                     ELSE jsonb_build_object('kind', r.kind, 'id', r.owner_id) END),
                    '{}'::jsonb, r.scope);
        END IF;
        UPDATE secrets SET spec = jsonb_set(spec, '{accountId}', to_jsonb(acc)) WHERE id = r.id;
    END LOOP;
END $$;
