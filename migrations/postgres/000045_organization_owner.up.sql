-- 000045: the global scope is named organization, and the rows that belong to the deployment rather than to its catalog are owned by it.
--
-- Moved here, where the owner alone decides: teams, groups, custom roles (every role but the built-ins, which stay system), role bindings at the global scope (scope_kind and the owner copy in metadata), policy bindings with no owner column (the metadata copy; the columns stay the truth), and system-owned accounts. Users carry no owner. Moved at boot, where the catalog tree or the master key is needed: system-owned policies, rate limits, budgets and pools the catalog does not ship, and host keys, whose stored values are sealed again under the organization's data key. Audit rows keep the owner they were written with.
--
-- No unique index encodes an owner here: accounts' default_owner and policy_binding_subjects' owner_scope were dropped by 000041 and 000039.

UPDATE teams SET metadata = jsonb_set(metadata, '{owner}', '{"kind": "organization"}')
WHERE metadata->'owner'->>'kind' = 'system';

UPDATE groups SET metadata = jsonb_set(metadata, '{owner}', '{"kind": "organization"}')
WHERE metadata->'owner'->>'kind' = 'system';

UPDATE roles SET metadata = jsonb_set(metadata, '{owner}', '{"kind": "organization"}')
WHERE metadata->'owner'->>'kind' = 'system'
  AND name NOT IN ('admin', 'auditor', 'catalog-editor', 'team-admin', 'project-admin', 'developer', 'viewer');

ALTER TABLE role_bindings DROP CONSTRAINT IF EXISTS role_bindings_scope_kind_check;
UPDATE role_bindings SET scope_kind = 'organization', metadata = jsonb_set(metadata, '{owner}', '{"kind": "organization"}')
WHERE scope_kind = 'system';
ALTER TABLE role_bindings ADD CONSTRAINT role_bindings_scope_kind_check CHECK (scope_kind IN ('organization', 'team', 'project'));

UPDATE policy_bindings SET metadata = jsonb_set(metadata, '{owner}', '{"kind": "organization"}')
WHERE project_id IS NULL AND team_id IS NULL AND user_id IS NULL;

UPDATE accounts SET metadata = jsonb_set(metadata, '{owner}', '{"kind": "organization"}')
WHERE metadata->'owner'->>'kind' = 'system';
