-- The directory tables. Every statement creates only what is missing, so on a database that already holds these tables in this shape the migration changes nothing.
--
-- Rows other than users keep identity in columns (id, name, display_name) and the rest in two JSONB documents: metadata (description, owner, labels, annotations, dirty) and spec. Relations that must cascade (team -> project -> service account, role -> binding, members and subjects) are real columns with foreign keys. Binding scopes are opaque: scope_kind carries no CHECK.

CREATE TABLE IF NOT EXISTS users (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    email         TEXT UNIQUE,
    password_hash TEXT,
    oidc_subject  TEXT UNIQUE,
    roles         TEXT[] NOT NULL DEFAULT '{}',
    disabled      BOOLEAN NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    token_version INT NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS teams (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL UNIQUE,
    display_name     TEXT NOT NULL DEFAULT '',
    metadata         JSONB NOT NULL,
    spec             JSONB NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resource_version BIGINT NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS projects (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL UNIQUE,
    display_name     TEXT NOT NULL DEFAULT '',
    team_id          TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    metadata         JSONB NOT NULL,
    spec             JSONB NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resource_version BIGINT NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS projects_team_idx ON projects (team_id);

CREATE TABLE IF NOT EXISTS service_accounts (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL UNIQUE,
    display_name     TEXT NOT NULL DEFAULT '',
    project_id       TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    metadata         JSONB NOT NULL,
    spec             JSONB NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resource_version BIGINT NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS service_accounts_project_idx ON service_accounts (project_id);

CREATE TABLE IF NOT EXISTS groups (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL UNIQUE,
    display_name     TEXT NOT NULL DEFAULT '',
    metadata         JSONB NOT NULL,
    spec             JSONB NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resource_version BIGINT NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS group_members (
    group_id TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    user_id  TEXT NOT NULL REFERENCES users(id)  ON DELETE CASCADE,
    position INT  NOT NULL,
    PRIMARY KEY (group_id, user_id)
);
CREATE INDEX IF NOT EXISTS group_members_user_idx ON group_members (user_id);

CREATE TABLE IF NOT EXISTS roles (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL UNIQUE,
    display_name     TEXT NOT NULL DEFAULT '',
    metadata         JSONB NOT NULL,
    spec             JSONB NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resource_version BIGINT NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS role_bindings (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL UNIQUE,
    display_name     TEXT NOT NULL DEFAULT '',
    role_id          TEXT NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    scope_kind       TEXT NOT NULL,
    scope_id         TEXT,
    metadata         JSONB NOT NULL,
    spec             JSONB NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resource_version BIGINT NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS role_bindings_scope_idx ON role_bindings (scope_kind, scope_id);

-- A subject with an id also fills the FK column of its kind, so deleting a user or service account drops it from every binding. Group subjects carry only a name: a group asserted by an identity provider has no row.
CREATE TABLE IF NOT EXISTS role_binding_subjects (
    binding_id      TEXT NOT NULL REFERENCES role_bindings(id)    ON DELETE CASCADE,
    kind            TEXT NOT NULL CHECK (kind IN ('user', 'group', 'serviceaccount')),
    subject_id      TEXT,
    subject_name    TEXT,
    subject_user_id TEXT REFERENCES users(id)             ON DELETE CASCADE,
    subject_sa_id   TEXT REFERENCES service_accounts(id)  ON DELETE CASCADE,
    position        INT  NOT NULL,
    PRIMARY KEY (binding_id, position),
    CHECK ((subject_id IS NULL) <> (subject_name IS NULL))
);
CREATE INDEX IF NOT EXISTS role_binding_subjects_user_idx ON role_binding_subjects (subject_user_id);
CREATE INDEX IF NOT EXISTS role_binding_subjects_sa_idx   ON role_binding_subjects (subject_sa_id);
CREATE INDEX IF NOT EXISTS role_binding_subjects_name_idx ON role_binding_subjects (subject_name);

-- resource_version moves on every UPDATE in the trigger, so no write path can forget it; a conditional upsert compares against it.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_proc
                    WHERE proname = 'bump_resource_version'
                      AND pronamespace = current_schema()::regnamespace) THEN
        CREATE FUNCTION bump_resource_version() RETURNS trigger AS $fn$
        BEGIN
            NEW.resource_version := OLD.resource_version + 1;
            RETURN NEW;
        END;
        $fn$ LANGUAGE plpgsql;
    END IF;
END $$;

DO $$
DECLARE
    t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['teams', 'projects', 'service_accounts', 'groups', 'roles', 'role_bindings'] LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_trigger
                        WHERE tgname = t || '_resource_version' AND tgrelid = t::regclass) THEN
            EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE ON %I FOR EACH ROW EXECUTE FUNCTION bump_resource_version()',
                           t || '_resource_version', t);
        END IF;
    END LOOP;
END $$;
