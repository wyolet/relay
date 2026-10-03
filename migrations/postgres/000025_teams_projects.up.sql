-- 000025: teams + projects.
--
-- A Team is the outer tenancy scope; a Project is the Team-owned unit that
-- owns request-authoring rows (policies, relay keys, user host keys) via
-- owner.kind=project in their metadata JSONB. Both are catalog kinds:
-- metadata envelope, NOTIFY trigger, seedable from YAML.
--
-- projects.team_id is a real column so deleting a Team cascades without
-- parsing JSONB.

CREATE TABLE IF NOT EXISTS teams (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL DEFAULT '',
    metadata     JSONB NOT NULL,
    spec         JSONB NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS projects (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL DEFAULT '',
    team_id      TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    metadata     JSONB NOT NULL,
    spec         JSONB NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS projects_team_idx ON projects (team_id);

-- The admin token used to stamp a user owner with no id on what it created.
-- Nobody owns such a row; the tenancy code reads it as personal and keeps it
-- out of shared pools, so it becomes system-owned. Keys are handled in 000026.
UPDATE secrets SET metadata = jsonb_set(metadata, '{owner}', '{"kind":"system"}'::jsonb)
 WHERE metadata->'owner'->>'kind' = 'user' AND coalesce(metadata->'owner'->>'id', '') = '';
UPDATE policies SET metadata = jsonb_set(metadata, '{owner}', '{"kind":"system"}'::jsonb)
 WHERE metadata->'owner'->>'kind' = 'user' AND coalesce(metadata->'owner'->>'id', '') = '';
UPDATE rate_limits SET metadata = jsonb_set(metadata, '{owner}', '{"kind":"system"}'::jsonb)
 WHERE metadata->'owner'->>'kind' = 'user' AND coalesce(metadata->'owner'->>'id', '') = '';
UPDATE hosts SET metadata = jsonb_set(metadata, '{owner}', '{"kind":"system"}'::jsonb)
 WHERE metadata->'owner'->>'kind' = 'user' AND coalesce(metadata->'owner'->>'id', '') = '';
UPDATE host_bindings SET metadata = jsonb_set(metadata, '{owner}', '{"kind":"system"}'::jsonb)
 WHERE metadata->'owner'->>'kind' = 'user' AND coalesce(metadata->'owner'->>'id', '') = '';
UPDATE models SET metadata = jsonb_set(metadata, '{owner}', '{"kind":"system"}'::jsonb)
 WHERE metadata->'owner'->>'kind' = 'user' AND coalesce(metadata->'owner'->>'id', '') = '';

CREATE TRIGGER teams_notify AFTER INSERT OR UPDATE OR DELETE ON teams
    FOR EACH ROW EXECUTE FUNCTION catalog_notify('team');
CREATE TRIGGER projects_notify AFTER INSERT OR UPDATE OR DELETE ON projects
    FOR EACH ROW EXECUTE FUNCTION catalog_notify('project');
