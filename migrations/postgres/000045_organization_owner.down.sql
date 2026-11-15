-- Every organization-owned row goes back to system, the only deployment owner schema 44 knows, including the policies, rate limits, budgets, pools and host keys the boot moved and any made since. Stored host key values sealed under the organization's data key would no longer open: run `relay secret organization-to-system` first, which this refuses to skip.

DO $$
DECLARE
    sealed BIGINT;
BEGIN
    SELECT count(*) INTO sealed FROM secret_values WHERE scope = 'organization';
    IF sealed > 0 THEN
        RAISE EXCEPTION 'migration 000045 down: % stored secrets are sealed under the organization data key; run `relay secret organization-to-system` first', sealed;
    END IF;
END
$$;

UPDATE teams SET metadata = jsonb_set(metadata, '{owner}', '{"kind": "system"}')
WHERE metadata->'owner'->>'kind' = 'organization';

UPDATE groups SET metadata = jsonb_set(metadata, '{owner}', '{"kind": "system"}')
WHERE metadata->'owner'->>'kind' = 'organization';

UPDATE roles SET metadata = jsonb_set(metadata, '{owner}', '{"kind": "system"}')
WHERE metadata->'owner'->>'kind' = 'organization';

ALTER TABLE role_bindings DROP CONSTRAINT IF EXISTS role_bindings_scope_kind_check;
UPDATE role_bindings SET scope_kind = 'system', metadata = jsonb_set(metadata, '{owner}', '{"kind": "system"}')
WHERE scope_kind = 'organization';
ALTER TABLE role_bindings ADD CONSTRAINT role_bindings_scope_kind_check CHECK (scope_kind IN ('system', 'team', 'project'));

UPDATE policy_bindings SET metadata = jsonb_set(metadata, '{owner}', '{"kind": "system"}')
WHERE metadata->'owner'->>'kind' = 'organization';

UPDATE accounts SET metadata = jsonb_set(metadata, '{owner}', '{"kind": "system"}')
WHERE metadata->'owner'->>'kind' = 'organization';

UPDATE policies SET metadata = jsonb_set(metadata, '{owner}', '{"kind": "system"}')
WHERE metadata->'owner'->>'kind' = 'organization';

UPDATE rate_limits SET metadata = jsonb_set(metadata, '{owner}', '{"kind": "system"}')
WHERE metadata->'owner'->>'kind' = 'organization';

UPDATE budgets SET metadata = jsonb_set(metadata, '{owner}', '{"kind": "system"}')
WHERE metadata->'owner'->>'kind' = 'organization';

UPDATE pools SET metadata = jsonb_set(metadata, '{owner}', '{"kind": "system"}')
WHERE metadata->'owner'->>'kind' = 'organization';

UPDATE secrets SET metadata = jsonb_set(metadata, '{owner}', '{"kind": "system"}')
WHERE metadata->'owner'->>'kind' = 'organization';
