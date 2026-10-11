-- Columns are listed, never *, so a column a product adds to a shared table does not break these reads.

-- name: GetUser :one
SELECT id, username, email, password_hash, oidc_subject, roles, disabled, created_at, updated_at, token_version
FROM users WHERE id = $1;

-- name: GetUserByUsername :one
SELECT id, username, email, password_hash, oidc_subject, roles, disabled, created_at, updated_at, token_version
FROM users WHERE username = $1;

-- name: GetUserByOIDCSubject :one
SELECT id, username, email, password_hash, oidc_subject, roles, disabled, created_at, updated_at, token_version
FROM users WHERE oidc_subject = $1;

-- name: ListUsers :many
SELECT id, username, email, password_hash, oidc_subject, roles, disabled, created_at, updated_at, token_version
FROM users ORDER BY created_at ASC, id ASC;

-- name: UpsertUser :exec
INSERT INTO users (id, username, email, password_hash, oidc_subject, roles, disabled)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (id) DO UPDATE SET
    username      = EXCLUDED.username,
    email         = EXCLUDED.email,
    password_hash = EXCLUDED.password_hash,
    oidc_subject  = EXCLUDED.oidc_subject,
    roles         = EXCLUDED.roles,
    disabled      = EXCLUDED.disabled,
    updated_at    = now();

-- name: DeleteUser :exec
DELETE FROM users WHERE id = $1;

-- name: BumpUserTokenVersion :exec
UPDATE users SET token_version = token_version + 1, updated_at = now() WHERE id = $1;

-- name: ListTeams :many
SELECT id, name, display_name, metadata, spec, created_at, updated_at, resource_version FROM teams ORDER BY name;

-- name: GetTeam :one
SELECT id, name, display_name, metadata, spec, created_at, updated_at, resource_version FROM teams WHERE id = $1;

-- name: UpsertTeam :execrows
INSERT INTO teams (id, name, display_name, metadata, spec, updated_at)
VALUES ($1, $2, $3, $4, $5, NOW())
ON CONFLICT (id) DO UPDATE SET
    name         = EXCLUDED.name,
    display_name = EXCLUDED.display_name,
    metadata     = EXCLUDED.metadata,
    spec         = EXCLUDED.spec,
    updated_at   = NOW()
WHERE sqlc.narg('expected_version')::bigint IS NULL OR teams.resource_version = sqlc.narg('expected_version')::bigint;

-- name: DeleteTeam :exec
DELETE FROM teams WHERE id = $1;

-- name: ListProjects :many
SELECT id, name, display_name, team_id, metadata, spec, created_at, updated_at, resource_version FROM projects ORDER BY name;

-- name: GetProject :one
SELECT id, name, display_name, team_id, metadata, spec, created_at, updated_at, resource_version FROM projects WHERE id = $1;

-- name: UpsertProject :execrows
INSERT INTO projects (id, name, display_name, team_id, metadata, spec, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, NOW())
ON CONFLICT (id) DO UPDATE SET
    name         = EXCLUDED.name,
    display_name = EXCLUDED.display_name,
    team_id      = EXCLUDED.team_id,
    metadata     = EXCLUDED.metadata,
    spec         = EXCLUDED.spec,
    updated_at   = NOW()
WHERE sqlc.narg('expected_version')::bigint IS NULL OR projects.resource_version = sqlc.narg('expected_version')::bigint;

-- name: DeleteProject :exec
DELETE FROM projects WHERE id = $1;

-- name: ListServiceAccounts :many
SELECT id, name, display_name, project_id, metadata, spec, created_at, updated_at, resource_version FROM service_accounts ORDER BY name;

-- name: GetServiceAccount :one
SELECT id, name, display_name, project_id, metadata, spec, created_at, updated_at, resource_version FROM service_accounts WHERE id = $1;

-- name: UpsertServiceAccount :execrows
INSERT INTO service_accounts (id, name, display_name, project_id, metadata, spec, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, NOW())
ON CONFLICT (id) DO UPDATE SET
    name         = EXCLUDED.name,
    display_name = EXCLUDED.display_name,
    project_id   = EXCLUDED.project_id,
    metadata     = EXCLUDED.metadata,
    spec         = EXCLUDED.spec,
    updated_at   = NOW()
WHERE sqlc.narg('expected_version')::bigint IS NULL OR service_accounts.resource_version = sqlc.narg('expected_version')::bigint;

-- name: DeleteServiceAccount :exec
DELETE FROM service_accounts WHERE id = $1;

-- name: ListGroups :many
SELECT id, name, display_name, metadata, spec, created_at, updated_at, resource_version FROM groups ORDER BY name;

-- name: GetGroup :one
SELECT id, name, display_name, metadata, spec, created_at, updated_at, resource_version FROM groups WHERE id = $1;

-- name: UpsertGroup :execrows
INSERT INTO groups (id, name, display_name, metadata, spec, updated_at)
VALUES ($1, $2, $3, $4, $5, NOW())
ON CONFLICT (id) DO UPDATE SET
    name         = EXCLUDED.name,
    display_name = EXCLUDED.display_name,
    metadata     = EXCLUDED.metadata,
    spec         = EXCLUDED.spec,
    updated_at   = NOW()
WHERE sqlc.narg('expected_version')::bigint IS NULL OR groups.resource_version = sqlc.narg('expected_version')::bigint;

-- name: DeleteGroup :exec
DELETE FROM groups WHERE id = $1;

-- name: ListGroupMembers :many
SELECT group_id, user_id, position FROM group_members ORDER BY group_id, position;

-- name: GetGroupMembers :many
SELECT group_id, user_id, position FROM group_members WHERE group_id = $1 ORDER BY position;

-- name: DeleteGroupMembers :exec
DELETE FROM group_members WHERE group_id = $1;

-- name: InsertGroupMember :exec
INSERT INTO group_members (group_id, user_id, position) VALUES ($1, $2, $3)
ON CONFLICT (group_id, user_id) DO UPDATE SET position = EXCLUDED.position;

-- name: ListRoles :many
SELECT id, name, display_name, metadata, spec, created_at, updated_at, resource_version FROM roles ORDER BY name;

-- name: GetRole :one
SELECT id, name, display_name, metadata, spec, created_at, updated_at, resource_version FROM roles WHERE id = $1;

-- name: UpsertRole :execrows
INSERT INTO roles (id, name, display_name, metadata, spec, updated_at)
VALUES ($1, $2, $3, $4, $5, NOW())
ON CONFLICT (id) DO UPDATE SET
    name         = EXCLUDED.name,
    display_name = EXCLUDED.display_name,
    metadata     = EXCLUDED.metadata,
    spec         = EXCLUDED.spec,
    updated_at   = NOW()
WHERE sqlc.narg('expected_version')::bigint IS NULL OR roles.resource_version = sqlc.narg('expected_version')::bigint;

-- name: DeleteRole :exec
DELETE FROM roles WHERE id = $1;

-- name: ListRoleBindings :many
SELECT id, name, display_name, role_id, scope_kind, scope_id, metadata, spec, created_at, updated_at, resource_version
FROM role_bindings ORDER BY name;

-- name: GetRoleBinding :one
SELECT id, name, display_name, role_id, scope_kind, scope_id, metadata, spec, created_at, updated_at, resource_version
FROM role_bindings WHERE id = $1;

-- name: UpsertRoleBinding :execrows
INSERT INTO role_bindings (id, name, display_name, role_id, scope_kind, scope_id, metadata, spec, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
ON CONFLICT (id) DO UPDATE SET
    name         = EXCLUDED.name,
    display_name = EXCLUDED.display_name,
    role_id      = EXCLUDED.role_id,
    scope_kind   = EXCLUDED.scope_kind,
    scope_id     = EXCLUDED.scope_id,
    metadata     = EXCLUDED.metadata,
    spec         = EXCLUDED.spec,
    updated_at   = NOW()
WHERE sqlc.narg('expected_version')::bigint IS NULL OR role_bindings.resource_version = sqlc.narg('expected_version')::bigint;

-- name: DeleteRoleBinding :exec
DELETE FROM role_bindings WHERE id = $1;

-- name: ListRoleBindingSubjects :many
SELECT binding_id, kind, subject_id, subject_name, position
FROM role_binding_subjects ORDER BY binding_id, position;

-- name: GetRoleBindingSubjects :many
SELECT binding_id, kind, subject_id, subject_name, position
FROM role_binding_subjects WHERE binding_id = $1 ORDER BY position;

-- name: DeleteRoleBindingSubjects :exec
DELETE FROM role_binding_subjects WHERE binding_id = $1;

-- name: InsertRoleBindingSubject :exec
INSERT INTO role_binding_subjects
    (binding_id, kind, subject_id, subject_name, subject_user_id, subject_sa_id, position)
VALUES ($1, $2, $3, $4, $5, $6, $7);
