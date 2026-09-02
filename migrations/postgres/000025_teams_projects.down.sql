-- Rows authored under a team or project outlive the tables that name them:
-- rewrite their owner to the pre-tenancy shape first, or the older binary
-- reads an owner kind it has no case for.
UPDATE policies
   SET metadata = jsonb_set(metadata, '{owner}', '{"kind":"user"}'::jsonb)
 WHERE metadata->'owner'->>'kind' IN ('team', 'project');
UPDATE secrets
   SET metadata = jsonb_set(metadata, '{owner}', '{"kind":"user"}'::jsonb)
 WHERE metadata->'owner'->>'kind' IN ('team', 'project');
UPDATE rate_limits
   SET metadata = jsonb_set(metadata, '{owner}', '{"kind":"user"}'::jsonb)
 WHERE metadata->'owner'->>'kind' IN ('team', 'project');

DROP TRIGGER IF EXISTS projects_notify ON projects;
DROP TRIGGER IF EXISTS teams_notify ON teams;
DROP TABLE IF EXISTS projects;
DROP TABLE IF EXISTS teams;
