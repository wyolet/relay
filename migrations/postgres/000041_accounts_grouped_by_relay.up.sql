-- 000041: a host key always names its provider account; relay no longer makes one for it.
--
-- The accounts relay made per owner and host (default_owner) grouped keys without knowing they share one provider org, so an upstream 429 stays with the key that met it. grouped_by_relay keeps that for those rows only, until an admin confirms the org by setting its orgId; no new account carries it. A key still without an account (its host was gone when relay grouped keys) joins one the same way first, so every key whose host exists names an account.

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

ALTER TABLE accounts ADD COLUMN IF NOT EXISTS grouped_by_relay BOOLEAN NOT NULL DEFAULT FALSE;
UPDATE accounts SET grouped_by_relay = TRUE
WHERE default_owner IS NOT NULL AND COALESCE(spec->>'orgId', '') = '';

-- These accounts are ordinary rows now: one no key names stays until it is deleted, as any account does.
DROP TRIGGER IF EXISTS teams_drop_keyless_default_accounts ON teams;
DROP TRIGGER IF EXISTS projects_drop_keyless_default_accounts ON projects;
DROP FUNCTION IF EXISTS accounts_drop_keyless_defaults();
DROP INDEX IF EXISTS accounts_one_default;
ALTER TABLE accounts DROP COLUMN IF EXISTS default_owner;
