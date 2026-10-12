-- Columns are listed, never *, so a column a product adds to a shared table does not break these reads.

-- name: GetUser :one
SELECT id, username, email, password_hash, oidc_subject, roles, disabled, created_at, updated_at, token_version, external_source, external_id, resource_version
FROM users WHERE id = $1;

-- name: GetUserByUsername :one
SELECT id, username, email, password_hash, oidc_subject, roles, disabled, created_at, updated_at, token_version, external_source, external_id, resource_version
FROM users WHERE username = $1;

-- name: GetUserByIdentity :one
SELECT id, username, email, password_hash, oidc_subject, roles, disabled, created_at, updated_at, token_version, external_source, external_id, resource_version
FROM users WHERE id = (SELECT user_id FROM identities WHERE issuer = $1 AND subject = $2);

-- name: ListUsersByExternalID :many
SELECT id, username, email, password_hash, oidc_subject, roles, disabled, created_at, updated_at, token_version, external_source, external_id, resource_version
FROM users WHERE external_source IS NOT NULL AND external_id = $1 ORDER BY created_at ASC, id ASC;

-- name: ListUsers :many
SELECT id, username, email, password_hash, oidc_subject, roles, disabled, created_at, updated_at, token_version, external_source, external_id, resource_version
FROM users ORDER BY created_at ASC, id ASC;

-- A new password, primary identity or disabled flag bumps the token version in the same statement, so no token outlives the change.
-- name: UpsertUser :execrows
INSERT INTO users (id, username, email, password_hash, oidc_subject, roles, disabled, external_source, external_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (id) DO UPDATE SET
    token_version   = CASE WHEN users.password_hash IS DISTINCT FROM EXCLUDED.password_hash
                             OR users.oidc_subject IS DISTINCT FROM EXCLUDED.oidc_subject
                             OR users.disabled <> EXCLUDED.disabled
                           THEN users.token_version + 1 ELSE users.token_version END,
    username        = EXCLUDED.username,
    email           = EXCLUDED.email,
    password_hash   = EXCLUDED.password_hash,
    oidc_subject    = EXCLUDED.oidc_subject,
    roles           = EXCLUDED.roles,
    disabled        = EXCLUDED.disabled,
    external_source = EXCLUDED.external_source,
    external_id     = EXCLUDED.external_id,
    updated_at      = now()
WHERE sqlc.narg('expected_version')::bigint IS NULL OR users.resource_version = sqlc.narg('expected_version')::bigint;

-- The same statement bumps the token version, so a re-enabled account never revives a token minted before.
-- name: SetUserDisabled :execrows
UPDATE users SET disabled = $2, token_version = token_version + 1, updated_at = now() WHERE id = $1 AND disabled <> $2;

-- Touches only the columns a provisioning source owns, and only on its own rows.
-- name: UpdateProvisionedUser :execrows
UPDATE users SET username = $3, email = $4, external_id = $5, updated_at = now() WHERE id = $1 AND external_source = $2;

-- name: DeleteUser :exec
DELETE FROM users WHERE id = $1;

-- name: BumpUserTokenVersion :exec
UPDATE users SET token_version = token_version + 1, updated_at = now() WHERE id = $1;

-- Identities read oldest first; that order also picks the next primary when the primary is removed.

-- name: ListUserIdentities :many
SELECT user_id, issuer, subject, external_id, created_at FROM identities WHERE user_id = $1 ORDER BY created_at, issuer, subject;

-- name: ListIdentities :many
SELECT user_id, issuer, subject, external_id, created_at FROM identities ORDER BY user_id, created_at, issuer, subject;

-- An identity another account holds matches no row, which the store reads as a conflict.
-- name: UpsertIdentity :execrows
INSERT INTO identities (user_id, issuer, subject, external_id) VALUES ($1, $2, $3, $4)
ON CONFLICT (issuer, subject) DO UPDATE SET external_id = EXCLUDED.external_id
WHERE identities.user_id = EXCLUDED.user_id;

-- name: SetPrimaryIdentityIfNone :exec
UPDATE users SET oidc_subject = $2 WHERE id = $1 AND oidc_subject IS NULL;

-- The trigger writes the identity row. A concurrent first link waits on the row lock, then finds oidc_subject set and matches nothing.
-- name: SetFirstPrimaryIdentity :execrows
UPDATE users SET oidc_subject = $2, token_version = token_version + 1, updated_at = now()
WHERE id = $1 AND oidc_subject IS NULL AND NOT EXISTS (SELECT 1 FROM identities WHERE identities.user_id = $1);

-- name: DeleteIdentity :exec
DELETE FROM identities WHERE user_id = $1 AND issuer = $2 AND subject = $3;

-- name: DeleteUserIdentities :exec
DELETE FROM identities WHERE user_id = $1;

-- name: ClearPrimaryIdentity :exec
UPDATE users SET oidc_subject = NULL WHERE id = $1;

-- Promotes only while the primary is still the one named, NULL for none; with no identity left the primary stays NULL.
-- name: PromoteOldestIdentity :exec
UPDATE users SET oidc_subject = (
    SELECT i.issuer || '|' || i.subject FROM identities i WHERE i.user_id = users.id
     ORDER BY i.created_at, i.issuer, i.subject LIMIT 1)
WHERE id = $1 AND oidc_subject IS NOT DISTINCT FROM $2;

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

-- name: DeleteTeamRoleBindings :exec
DELETE FROM role_bindings
WHERE (scope_kind = sqlc.arg('team_kind')::text AND scope_id = sqlc.arg('team_id')::text)
   OR (scope_kind = sqlc.arg('project_kind')::text AND scope_id IN (SELECT id FROM projects WHERE team_id = sqlc.arg('team_id')::text));

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

-- name: DeleteProjectRoleBindings :exec
DELETE FROM role_bindings WHERE scope_kind = sqlc.arg('project_kind')::text AND scope_id = sqlc.arg('project_id')::text;

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
SELECT id, name, display_name, metadata, spec, created_at, updated_at, resource_version, external_source, external_id FROM groups ORDER BY name;

-- name: GetGroup :one
SELECT id, name, display_name, metadata, spec, created_at, updated_at, resource_version, external_source, external_id FROM groups WHERE id = $1;

-- name: UpsertGroup :execrows
INSERT INTO groups (id, name, display_name, metadata, spec, external_source, external_id, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
ON CONFLICT (id) DO UPDATE SET
    name            = EXCLUDED.name,
    display_name    = EXCLUDED.display_name,
    metadata        = EXCLUDED.metadata,
    spec            = EXCLUDED.spec,
    external_source = EXCLUDED.external_source,
    external_id     = EXCLUDED.external_id,
    updated_at      = NOW()
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

-- name: GetSCIMClientByTokenHash :one
SELECT name, token_hash, token_prefix, created_at, last_used_at, revoked_at FROM scim_clients WHERE token_hash = $1;

-- name: GetSCIMClient :one
SELECT name, token_hash, token_prefix, created_at, last_used_at, revoked_at FROM scim_clients WHERE name = $1;

-- name: ListSCIMClients :many
SELECT name, token_hash, token_prefix, created_at, last_used_at, revoked_at FROM scim_clients ORDER BY name;

-- A name is never issued twice, revoked or not, so a new token never inherits the rows an old one provisioned; the insert touches no row.
-- name: CreateSCIMClient :execrows
INSERT INTO scim_clients (name, token_hash, token_prefix)
VALUES ($1, $2, $3)
ON CONFLICT (name) DO NOTHING;

-- Writes at most once a minute per client, so a burst of provisioning calls is not a burst of updates.
-- name: MarkSCIMClientUsed :exec
UPDATE scim_clients SET last_used_at = sqlc.arg(used_at)::timestamptz
WHERE name = sqlc.arg(name) AND (last_used_at IS NULL OR last_used_at < sqlc.arg(used_at)::timestamptz - interval '1 minute');

-- name: RotateSCIMClient :execrows
UPDATE scim_clients SET token_hash = $2, token_prefix = $3, last_used_at = NULL WHERE name = $1 AND revoked_at IS NULL;

-- name: RevokeSCIMClient :execrows
UPDATE scim_clients SET revoked_at = now() WHERE name = $1 AND revoked_at IS NULL;
