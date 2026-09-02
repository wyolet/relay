-- 000027: roles, role bindings, policy bindings.
--
-- A Role is a global rule set; a RoleBinding grants it to subjects at one
-- scope (global | team | project); a PolicyBinding points subjects inside a
-- Project at one Policy. The rows are stored, validated and indexed now;
-- nothing evaluates them yet.
--
-- Subjects live in junction tables rather than the spec JSONB so a deleted
-- user or service account disappears from every binding through the FK.
-- Group subjects carry only a name: an IdP group has no row to reference.

CREATE TABLE IF NOT EXISTS roles (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL DEFAULT '',
    metadata     JSONB NOT NULL,
    spec         JSONB NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS role_bindings (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL DEFAULT '',
    role_id      TEXT NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    scope_kind   TEXT NOT NULL CHECK (scope_kind IN ('system', 'team', 'project')),
    scope_id     TEXT,
    metadata     JSONB NOT NULL,
    spec         JSONB NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS role_bindings_scope_idx ON role_bindings (scope_kind, scope_id);

CREATE TABLE IF NOT EXISTS policy_bindings (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL DEFAULT '',
    project_id   TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    policy_id    TEXT NOT NULL REFERENCES policies(id) ON DELETE CASCADE,
    priority     INT  NOT NULL DEFAULT 100,
    metadata     JSONB NOT NULL,
    spec         JSONB NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS policy_bindings_project_idx ON policy_bindings (project_id);

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

CREATE TABLE IF NOT EXISTS policy_binding_subjects (
    binding_id      TEXT NOT NULL REFERENCES policy_bindings(id)  ON DELETE CASCADE,
    kind            TEXT NOT NULL CHECK (kind IN ('user', 'group', 'serviceaccount')),
    subject_id      TEXT,
    subject_name    TEXT,
    subject_user_id TEXT REFERENCES users(id)             ON DELETE CASCADE,
    subject_sa_id   TEXT REFERENCES service_accounts(id)  ON DELETE CASCADE,
    position        INT  NOT NULL,
    PRIMARY KEY (binding_id, position),
    CHECK ((subject_id IS NULL) <> (subject_name IS NULL))
);
CREATE INDEX IF NOT EXISTS policy_binding_subjects_user_idx ON policy_binding_subjects (subject_user_id);
CREATE INDEX IF NOT EXISTS policy_binding_subjects_sa_idx   ON policy_binding_subjects (subject_sa_id);
CREATE INDEX IF NOT EXISTS policy_binding_subjects_name_idx ON policy_binding_subjects (subject_name);

CREATE TRIGGER roles_notify AFTER INSERT OR UPDATE OR DELETE ON roles
    FOR EACH ROW EXECUTE FUNCTION catalog_notify('role');
CREATE TRIGGER role_bindings_notify AFTER INSERT OR UPDATE OR DELETE ON role_bindings
    FOR EACH ROW EXECUTE FUNCTION catalog_notify('rolebinding');
CREATE TRIGGER policy_bindings_notify AFTER INSERT OR UPDATE OR DELETE ON policy_bindings
    FOR EACH ROW EXECUTE FUNCTION catalog_notify('policybinding');
CREATE TRIGGER role_binding_subjects_notify AFTER INSERT OR UPDATE OR DELETE ON role_binding_subjects
    FOR EACH ROW EXECUTE FUNCTION catalog_notify_junction('rolebinding', 'binding_id');
CREATE TRIGGER policy_binding_subjects_notify AFTER INSERT OR UPDATE OR DELETE ON policy_binding_subjects
    FOR EACH ROW EXECUTE FUNCTION catalog_notify_junction('policybinding', 'binding_id');
