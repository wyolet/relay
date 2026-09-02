-- 000026: service accounts, groups, and the Key principal.
--
-- A Key stops being an identity and becomes a credential OF a principal:
-- either a ServiceAccount (which lives in a Project) or a User. The two
-- principal columns are real FKs so deleting either end cascades without
-- parsing JSONB, and the CHECK keeps exactly one of them set.
--
-- Existing relay_keys are backfilled: user-owned rows whose owner id is a
-- real user become user principals; everything else is re-parented onto a
-- generated ServiceAccount inside a system Project `legacy`, so no key
-- stops working. Operators re-parent those from the UI afterwards.

CREATE TABLE IF NOT EXISTS service_accounts (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL DEFAULT '',
    project_id   TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    metadata     JSONB NOT NULL,
    spec         JSONB NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS service_accounts_project_idx ON service_accounts (project_id);

CREATE TABLE IF NOT EXISTS groups (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL DEFAULT '',
    metadata     JSONB NOT NULL,
    spec         JSONB NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS group_members (
    group_id TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    user_id  TEXT NOT NULL REFERENCES users(id)  ON DELETE CASCADE,
    position INT  NOT NULL,
    PRIMARY KEY (group_id, user_id)
);
CREATE INDEX IF NOT EXISTS group_members_user_idx ON group_members (user_id);

ALTER TABLE relay_keys
    ADD COLUMN IF NOT EXISTS principal_sa_id   TEXT REFERENCES service_accounts(id) ON DELETE CASCADE,
    ADD COLUMN IF NOT EXISTS principal_user_id TEXT REFERENCES users(id)            ON DELETE CASCADE,
    ADD COLUMN IF NOT EXISTS previous_key_hash TEXT;

-- Backfill 1: a key already owned by a real user keeps that user as its
-- principal.
UPDATE relay_keys k
   SET principal_user_id = u.id,
       spec = jsonb_set(k.spec, '{principal}',
                        jsonb_build_object('kind', 'user', 'id', u.id), true)
  FROM users u
 WHERE k.principal_sa_id IS NULL
   AND k.principal_user_id IS NULL
   AND k.metadata->'owner'->>'kind' = 'user'
   AND (k.metadata->'owner'->>'id' = u.id
     OR k.metadata->'owner'->>'id' = u.username);

-- Backfill 2: every remaining key gets a generated ServiceAccount in a
-- system Project `legacy`. The tenancy rows are keyed by name, not by a
-- fixed id: `name` is what carries the UNIQUE constraint, so an operator
-- who already has a team named `system` keeps it and the backfill hangs
-- its rows off that id instead of failing on the duplicate name.
INSERT INTO teams (id, name, display_name, metadata, spec)
SELECT gen_random_uuid()::text, 'system', 'System',
       '{"owner":{"kind":"system"}}'::jsonb, '{}'::jsonb
 WHERE EXISTS (SELECT 1 FROM relay_keys
                WHERE principal_sa_id IS NULL AND principal_user_id IS NULL)
ON CONFLICT (name) DO NOTHING;

INSERT INTO projects (id, name, display_name, team_id, metadata, spec)
SELECT gen_random_uuid()::text, 'legacy', 'Legacy', t.id,
       jsonb_build_object('owner', jsonb_build_object('kind', 'team', 'id', t.id)),
       jsonb_build_object('teamId', t.id)
  FROM teams t
 WHERE t.name = 'system'
   AND EXISTS (SELECT 1 FROM relay_keys
                WHERE principal_sa_id IS NULL AND principal_user_id IS NULL)
ON CONFLICT (name) DO NOTHING;

INSERT INTO service_accounts (id, name, display_name, project_id, metadata, spec)
SELECT gen_random_uuid()::text, left('legacy-' || k.name, 63), k.display_name, p.id,
       jsonb_build_object('owner', jsonb_build_object('kind', 'project', 'id', p.id)),
       jsonb_build_object('projectId', p.id)
  FROM relay_keys k
 CROSS JOIN projects p
 WHERE p.name = 'legacy'
   AND k.principal_sa_id IS NULL AND k.principal_user_id IS NULL
ON CONFLICT (name) DO NOTHING;

UPDATE relay_keys k
   SET principal_sa_id = sa.id,
       spec = jsonb_set(k.spec, '{principal}',
                        jsonb_build_object('kind', 'serviceaccount', 'id', sa.id), true),
       metadata = jsonb_set(k.metadata, '{owner}',
                            jsonb_build_object('kind', 'project', 'id', sa.project_id), true)
  FROM service_accounts sa
 WHERE k.principal_sa_id IS NULL
   AND k.principal_user_id IS NULL
   AND sa.name = left('legacy-' || k.name, 63);

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'relay_keys_one_principal') THEN
        ALTER TABLE relay_keys
            ADD CONSTRAINT relay_keys_one_principal
            CHECK ((principal_sa_id IS NULL) <> (principal_user_id IS NULL));
    END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS relay_keys_previous_hash_idx
    ON relay_keys (previous_key_hash) WHERE previous_key_hash IS NOT NULL;

CREATE TRIGGER service_accounts_notify AFTER INSERT OR UPDATE OR DELETE ON service_accounts
    FOR EACH ROW EXECUTE FUNCTION catalog_notify('serviceaccount');
CREATE TRIGGER groups_notify AFTER INSERT OR UPDATE OR DELETE ON groups
    FOR EACH ROW EXECUTE FUNCTION catalog_notify('group');
CREATE TRIGGER group_members_notify AFTER INSERT OR UPDATE OR DELETE ON group_members
    FOR EACH ROW EXECUTE FUNCTION catalog_notify_junction('group', 'group_id');
