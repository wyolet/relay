-- name: ListProviders :many
SELECT id, name, display_name, metadata, spec, created_at, updated_at FROM providers ORDER BY name;

-- name: ListPolicies :many
SELECT id, name, display_name, metadata, spec, created_at, updated_at FROM policies ORDER BY name;

-- name: ListSecrets :many
SELECT id, name, display_name, metadata, spec, status, value_kind, value_from_env, value_ciphertext, value_nonce, value_key_version, created_at, updated_at FROM secrets ORDER BY name;

-- name: ListStoredSecretsForRotation :many
SELECT id, value_ciphertext, value_nonce, value_key_version FROM secrets WHERE value_kind = 'stored' ORDER BY id;

-- name: UpdateSecretCiphertext :exec
UPDATE secrets
SET value_ciphertext  = $2,
    value_nonce       = $3,
    value_key_version = $4,
    updated_at        = NOW()
WHERE id = $1 AND value_kind = 'stored';

-- name: ListModels :many
SELECT id, name, display_name, metadata, spec, created_at, updated_at FROM models ORDER BY name;

-- name: ListRateLimits :many
SELECT id, name, display_name, metadata, spec, created_at, updated_at FROM rate_limits ORDER BY name;

-- name: UpsertProvider :exec
INSERT INTO providers (id, name, display_name, metadata, spec, updated_at)
VALUES ($1, $2, $3, $4, $5, NOW())
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name,
    display_name = EXCLUDED.display_name,
    metadata = EXCLUDED.metadata,
    spec = EXCLUDED.spec,
    updated_at = NOW();

-- name: UpsertPolicy :exec
INSERT INTO policies (id, name, display_name, metadata, spec, models, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, NOW())
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name,
    display_name = EXCLUDED.display_name,
    metadata = EXCLUDED.metadata,
    spec = EXCLUDED.spec,
    models = EXCLUDED.models,
    updated_at = NOW();

-- UpsertSecret is kept for the seed CLI (YAML-import path). Deprecated for new code; use InsertSecretEnv / InsertSecretStored.
-- name: UpsertSecret :exec
INSERT INTO secrets (id, name, display_name, metadata, spec, updated_at)
VALUES ($1, $2, $3, $4, $5, NOW())
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name,
    display_name = EXCLUDED.display_name,
    metadata = EXCLUDED.metadata,
    spec = EXCLUDED.spec,
    updated_at = NOW();

-- name: InsertSecretEnv :one
INSERT INTO secrets (id, name, display_name, value_kind, value_from_env, metadata, spec)
VALUES ($1, $2, $3, 'env', $4, $5, $6)
ON CONFLICT (id) DO UPDATE
    SET name              = EXCLUDED.name,
        display_name      = EXCLUDED.display_name,
        value_kind        = 'env',
        value_from_env    = EXCLUDED.value_from_env,
        value_ciphertext  = NULL,
        value_nonce       = NULL,
        value_key_version = NULL,
        metadata          = EXCLUDED.metadata,
        spec              = EXCLUDED.spec,
        updated_at        = NOW()
RETURNING id, name, display_name, value_kind, value_from_env, value_ciphertext, value_nonce, value_key_version, metadata, spec;

-- name: InsertSecretStored :one
INSERT INTO secrets (id, name, display_name, value_kind, value_ciphertext, value_nonce, value_key_version, metadata, spec)
VALUES ($1, $2, $3, 'stored', $4, $5, $6, $7, $8)
ON CONFLICT (id) DO UPDATE
    SET name              = EXCLUDED.name,
        display_name      = EXCLUDED.display_name,
        value_kind        = 'stored',
        value_from_env    = NULL,
        value_ciphertext  = EXCLUDED.value_ciphertext,
        value_nonce       = EXCLUDED.value_nonce,
        value_key_version = EXCLUDED.value_key_version,
        metadata          = EXCLUDED.metadata,
        spec              = EXCLUDED.spec,
        updated_at        = NOW()
RETURNING id, name, display_name, value_kind, value_from_env, value_ciphertext, value_nonce, value_key_version, metadata, spec;

-- name: UpdateSecretEnv :one
UPDATE secrets
SET value_kind        = 'env',
    value_from_env    = $2,
    value_ciphertext  = NULL,
    value_nonce       = NULL,
    value_key_version = NULL,
    updated_at        = NOW()
WHERE id = $1
RETURNING id, name, display_name, value_kind, value_from_env, value_ciphertext, value_nonce, value_key_version, metadata, spec;

-- name: UpdateSecretStored :one
UPDATE secrets
SET value_kind        = 'stored',
    value_from_env    = NULL,
    value_ciphertext  = $2,
    value_nonce       = $3,
    value_key_version = $4,
    status            = NULL,
    updated_at        = NOW()
WHERE id = $1
RETURNING id, name, display_name, value_kind, value_from_env, value_ciphertext, value_nonce, value_key_version, metadata, spec;

-- name: DeleteSecret :exec
DELETE FROM secrets WHERE id = $1;

-- InsertSecretStoredRef upserts a stored-mode HostKey row WITHOUT inline
-- ciphertext — the encrypted value lives in secret_values (pkg/secret),
-- keyed by this row's id. Replaces InsertSecretStored on the new path.
-- name: InsertSecretStoredRef :one
INSERT INTO secrets (id, name, display_name, value_kind, metadata, spec)
VALUES ($1, $2, $3, 'stored', $4, $5)
ON CONFLICT (id) DO UPDATE
    SET name              = EXCLUDED.name,
        display_name      = EXCLUDED.display_name,
        value_kind        = 'stored',
        value_from_env    = NULL,
        value_ciphertext  = NULL,
        value_nonce       = NULL,
        value_key_version = NULL,
        metadata          = EXCLUDED.metadata,
        spec              = EXCLUDED.spec,
        updated_at        = NOW()
RETURNING id, name, display_name, value_kind, value_from_env, value_ciphertext, value_nonce, value_key_version, metadata, spec;

-- secret_values: generic stored-secret value store (pkg/secret "stored").

-- name: GetSecretValue :one
SELECT ciphertext, nonce, key_version FROM secret_values WHERE id = $1;

-- name: UpsertSecretValue :exec
INSERT INTO secret_values (id, ciphertext, nonce, key_version, updated_at)
VALUES ($1, $2, $3, $4, NOW())
ON CONFLICT (id) DO UPDATE SET
    ciphertext  = EXCLUDED.ciphertext,
    nonce       = EXCLUDED.nonce,
    key_version = EXCLUDED.key_version,
    updated_at  = NOW();

-- name: ListSecretValuesForRotation :many
SELECT id, ciphertext, nonce, key_version FROM secret_values ORDER BY id;

-- name: DeleteSecretValue :exec
DELETE FROM secret_values WHERE id = $1;

-- name: MaxSecretValueKeyVersion :one
SELECT COALESCE(MAX(key_version), 0)::int FROM secret_values;

-- name: UpsertModel :exec
INSERT INTO models (id, name, display_name, metadata, spec, updated_at)
VALUES ($1, $2, $3, $4, $5, NOW())
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name,
    display_name = EXCLUDED.display_name,
    metadata = EXCLUDED.metadata,
    spec = EXCLUDED.spec,
    updated_at = NOW();

-- name: UpsertRateLimit :exec
INSERT INTO rate_limits (id, name, display_name, metadata, spec, updated_at)
VALUES ($1, $2, $3, $4, $5, NOW())
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name,
    display_name = EXCLUDED.display_name,
    metadata = EXCLUDED.metadata,
    spec = EXCLUDED.spec,
    updated_at = NOW();

-- name: DeleteProvider :exec
DELETE FROM providers WHERE id = $1;

-- name: DeletePolicy :exec
DELETE FROM policies WHERE id = $1;

-- name: DeleteModel :exec
DELETE FROM models WHERE id = $1;

-- name: DeleteRateLimit :exec
DELETE FROM rate_limits WHERE id = $1;

-- name: ListRelayKeys :many
SELECT id, name, display_name, key_hash, previous_key_hash, principal_sa_id, principal_user_id, metadata, spec, created_at, updated_at FROM relay_keys ORDER BY name;

-- name: UpsertRelayKey :exec
INSERT INTO relay_keys (id, name, display_name, key_hash, previous_key_hash, principal_sa_id, principal_user_id, metadata, spec, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW())
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name,
    display_name = EXCLUDED.display_name,
    key_hash          = EXCLUDED.key_hash,
    previous_key_hash = EXCLUDED.previous_key_hash,
    principal_sa_id   = EXCLUDED.principal_sa_id,
    principal_user_id = EXCLUDED.principal_user_id,
    metadata   = EXCLUDED.metadata,
    spec       = EXCLUDED.spec,
    updated_at = NOW();

-- name: DeleteRelayKey :exec
DELETE FROM relay_keys WHERE id = $1;

-- ── app/ arch (migration 0009) ───────────────────────────────────────────────

-- name: ListHosts :many
SELECT id, name, display_name, metadata, spec, created_at, updated_at FROM hosts ORDER BY name;

-- name: UpsertHost :exec
INSERT INTO hosts (id, name, display_name, metadata, spec, updated_at)
VALUES ($1, $2, $3, $4, $5, NOW())
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name,
    display_name = EXCLUDED.display_name,
    metadata = EXCLUDED.metadata,
    spec = EXCLUDED.spec,
    updated_at = NOW();

-- name: DeleteHost :exec
DELETE FROM hosts WHERE id = $1;

-- name: SetPolicyRateLimit :exec
UPDATE policies SET rate_limit_id = $2, updated_at = NOW() WHERE id = $1;

-- name: ListPolicyModels :many
SELECT policy_id, model_id, position FROM policy_models ORDER BY policy_id, position;

-- name: DeletePolicyModels :exec
DELETE FROM policy_models WHERE policy_id = $1;

-- name: InsertPolicyModel :exec
INSERT INTO policy_models (policy_id, model_id, position) VALUES ($1, $2, $3);

-- name: ListPolicyHostKeys :many
SELECT policy_id, host_key_id, position FROM policy_host_keys ORDER BY policy_id, position;

-- name: DeletePolicyHostKeys :exec
DELETE FROM policy_host_keys WHERE policy_id = $1;

-- name: InsertPolicyHostKey :exec
INSERT INTO policy_host_keys (policy_id, host_key_id, position) VALUES ($1, $2, $3);

-- name: ListPoliciesWithRateLimit :many
SELECT id, name, display_name, metadata, spec, rate_limit_id, models, created_at, updated_at FROM policies ORDER BY name;

-- ── pricing (migration 0010) ─────────────────────────────────────────────────

-- name: ListPricings :many
SELECT id, name, display_name, host_id, metadata, spec, created_at, updated_at FROM pricings ORDER BY name;

-- name: UpsertPricing :exec
INSERT INTO pricings (id, name, display_name, host_id, metadata, spec, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, NOW())
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name,
    display_name = EXCLUDED.display_name,
    host_id = EXCLUDED.host_id,
    metadata = EXCLUDED.metadata,
    spec = EXCLUDED.spec,
    updated_at = NOW();

-- name: DeletePricing :exec
DELETE FROM pricings WHERE id = $1;

-- name: ListPricingModels :many
SELECT pricing_id, model_id, position FROM pricing_models ORDER BY pricing_id, position;

-- name: DeletePricingModels :exec
DELETE FROM pricing_models WHERE pricing_id = $1;

-- name: InsertPricingModel :exec
INSERT INTO pricing_models (pricing_id, model_id, position) VALUES ($1, $2, $3);

-- ── host_bindings (migration 0020) ───────────────────────────────────────────

-- name: ListHostBindings :many
SELECT id, name, display_name, model_id, host_id, pricing_id, metadata, spec, created_at, updated_at FROM host_bindings ORDER BY name;

-- name: GetHostBinding :one
SELECT id, name, display_name, model_id, host_id, pricing_id, metadata, spec, created_at, updated_at FROM host_bindings WHERE id = $1;

-- name: UpsertHostBinding :exec
INSERT INTO host_bindings (id, name, display_name, model_id, host_id, pricing_id, metadata, spec, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name,
    display_name = EXCLUDED.display_name,
    model_id = EXCLUDED.model_id,
    host_id = EXCLUDED.host_id,
    pricing_id = EXCLUDED.pricing_id,
    metadata = EXCLUDED.metadata,
    spec = EXCLUDED.spec,
    updated_at = NOW();

-- name: DeleteHostBinding :exec
DELETE FROM host_bindings WHERE id = $1;

-- name: GetProvider :one
SELECT id, name, display_name, metadata, spec, created_at, updated_at FROM providers WHERE id = $1;

-- name: GetHost :one
SELECT id, name, display_name, metadata, spec, created_at, updated_at FROM hosts WHERE id = $1;

-- name: GetModel :one
SELECT id, name, display_name, metadata, spec, created_at, updated_at FROM models WHERE id = $1;

-- name: GetSecret :one
SELECT id, name, display_name, metadata, spec, status, value_kind, value_from_env, value_ciphertext, value_nonce, value_key_version, created_at, updated_at FROM secrets WHERE id = $1;

-- name: GetRateLimit :one
SELECT id, name, display_name, metadata, spec, created_at, updated_at FROM rate_limits WHERE id = $1;

-- name: GetPolicy :one
SELECT id, name, display_name, metadata, spec, rate_limit_id, models, created_at, updated_at FROM policies WHERE id = $1;

-- name: SetPolicyModels :exec
UPDATE policies SET models = $2, updated_at = NOW() WHERE id = $1;

-- name: GetPricing :one
SELECT id, name, display_name, host_id, metadata, spec, created_at, updated_at FROM pricings WHERE id = $1;

-- name: GetRelayKey :one
SELECT id, name, display_name, key_hash, previous_key_hash, principal_sa_id, principal_user_id, metadata, spec, created_at, updated_at FROM relay_keys WHERE id = $1;

-- name: GetPolicyModels :many
SELECT policy_id, model_id, position FROM policy_models WHERE policy_id = $1 ORDER BY position;

-- name: GetPricingModels :many
SELECT pricing_id, model_id, position FROM pricing_models WHERE pricing_id = $1 ORDER BY position;

-- name: GetPolicyHostKeys :many
SELECT policy_id, host_key_id, position FROM policy_host_keys WHERE policy_id = $1 ORDER BY position;


-- name: ListSettings :many
SELECT section, value, updated_at FROM settings ORDER BY section;

-- name: GetSetting :one
SELECT section, value, updated_at FROM settings WHERE section = $1;

-- name: UpsertSetting :exec
INSERT INTO settings (section, value, updated_at)
VALUES ($1, $2, NOW())
ON CONFLICT (section) DO UPDATE SET
    value = EXCLUDED.value,
    updated_at = NOW();

-- name: DeleteSetting :exec
DELETE FROM settings WHERE section = $1;

-- ===== batches =====

-- name: CreateBatch :exec
INSERT INTO batches (id, relay_key_hash, policy_id, inbound_shape, status, total_items,
                     project_id, team_id, principal_kind, principal_id, credential_kind, credential_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12);

-- name: GetBatch :one
SELECT id, relay_key_hash, policy_id, inbound_shape, status, total_items, created_at, completed_at,
       project_id, team_id, principal_kind, principal_id, credential_kind, credential_id
FROM batches WHERE id = $1;

-- name: ListBatchesByRelayKey :many
SELECT id, relay_key_hash, policy_id, inbound_shape, status, total_items, created_at, completed_at,
       project_id, team_id, principal_kind, principal_id, credential_kind, credential_id
FROM batches WHERE relay_key_hash = $1 ORDER BY created_at DESC;

-- name: SetBatchStatus :exec
UPDATE batches SET status = $2 WHERE id = $1;

-- name: SetBatchCompleted :exec
UPDATE batches SET status = $2, completed_at = NOW() WHERE id = $1;

-- name: CreateBatchItem :exec
INSERT INTO batch_items (batch_id, idx, job_id) VALUES ($1, $2, $3);

-- name: ListBatchItems :many
SELECT batch_id, idx, job_id FROM batch_items WHERE batch_id = $1 ORDER BY idx ASC;

-- name: ListOverlays :many
SELECT kind, resource_id, patch, updated_at FROM overlays ORDER BY kind, resource_id;

-- name: GetOverlay :one
SELECT kind, resource_id, patch, updated_at FROM overlays WHERE kind = $1 AND resource_id = $2;

-- name: UpsertOverlay :exec
INSERT INTO overlays (kind, resource_id, patch, updated_at)
VALUES ($1, $2, $3, NOW())
ON CONFLICT (kind, resource_id) DO UPDATE SET
    patch = EXCLUDED.patch,
    updated_at = NOW();

-- name: DeleteOverlay :exec
DELETE FROM overlays WHERE kind = $1 AND resource_id = $2;

-- name: GetUser :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserByUsername :one
SELECT * FROM users WHERE username = $1;

-- name: GetUserByOIDCSubject :one
SELECT * FROM users WHERE oidc_subject = $1;

-- name: ListUsers :many
SELECT * FROM users ORDER BY created_at ASC;

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

-- name: ListUserTokenVersions :many
SELECT id, token_version FROM users;

-- name: BumpUserTokenVersion :exec
UPDATE users SET token_version = token_version + 1, updated_at = now() WHERE id = $1;

-- name: UpdateSecretStatus :exec
UPDATE secrets SET status = $2 WHERE id = $1;

-- ── teams + projects (migration 0025) ────────────────────────────────────────

-- name: ListTeams :many
SELECT id, name, display_name, metadata, spec, created_at, updated_at FROM teams ORDER BY name;

-- name: GetTeam :one
SELECT id, name, display_name, metadata, spec, created_at, updated_at FROM teams WHERE id = $1;

-- name: UpsertTeam :exec
INSERT INTO teams (id, name, display_name, metadata, spec, updated_at)
VALUES ($1, $2, $3, $4, $5, NOW())
ON CONFLICT (id) DO UPDATE SET
    name         = EXCLUDED.name,
    display_name = EXCLUDED.display_name,
    metadata     = EXCLUDED.metadata,
    spec         = EXCLUDED.spec,
    updated_at   = NOW();

-- name: DeleteTeam :exec
DELETE FROM teams WHERE id = $1;

-- name: ListProjects :many
SELECT id, name, display_name, team_id, metadata, spec, created_at, updated_at FROM projects ORDER BY name;

-- name: GetProject :one
SELECT id, name, display_name, team_id, metadata, spec, created_at, updated_at FROM projects WHERE id = $1;

-- name: UpsertProject :exec
INSERT INTO projects (id, name, display_name, team_id, metadata, spec, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, NOW())
ON CONFLICT (id) DO UPDATE SET
    name         = EXCLUDED.name,
    display_name = EXCLUDED.display_name,
    team_id      = EXCLUDED.team_id,
    metadata     = EXCLUDED.metadata,
    spec         = EXCLUDED.spec,
    updated_at   = NOW();

-- name: DeleteProject :exec
DELETE FROM projects WHERE id = $1;

-- ── audit events (migration 0028) ────────────────────────────────────────────

-- name: InsertAuditEvent :copyfrom
INSERT INTO audit_events (
    id, ts, actor_kind, actor_id, actor_name, session_id, ip,
    action, resource_kind, resource_id, resource_name, owner_kind, owner_id,
    scope, status, code, request_id, method, path, changed_fields
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20
);

-- name: ListAuditEvents :many
SELECT id, ts, actor_kind, actor_id, actor_name, session_id, ip,
       action, resource_kind, resource_id, resource_name, owner_kind, owner_id,
       scope, status, code, request_id, method, path, changed_fields
FROM audit_events
WHERE (sqlc.narg('actor_id')::text IS NULL OR actor_id = sqlc.narg('actor_id')::text)
  AND (sqlc.narg('actor_name')::text IS NULL OR actor_name = sqlc.narg('actor_name')::text)
  AND (cardinality(@actions::text[]) = 0 OR action = ANY(@actions::text[]))
  AND (cardinality(@resource_kinds::text[]) = 0 OR resource_kind = ANY(@resource_kinds::text[]))
  AND (sqlc.narg('resource_id')::text IS NULL OR resource_id = sqlc.narg('resource_id')::text)
  AND (cardinality(@scopes::text[]) = 0 OR scope && @scopes::text[])
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status')::text)
  AND (sqlc.narg('from_ts')::timestamptz IS NULL OR ts >= sqlc.narg('from_ts')::timestamptz)
  AND (sqlc.narg('to_ts')::timestamptz IS NULL OR ts <= sqlc.narg('to_ts')::timestamptz)
  AND (sqlc.narg('cursor_ts')::timestamptz IS NULL
       OR (ts, id) < (sqlc.narg('cursor_ts')::timestamptz, @cursor_id::text))
ORDER BY ts DESC, id DESC
LIMIT @row_limit;

-- name: PruneAuditEvents :execrows
DELETE FROM audit_events WHERE ts < $1;
-- ── service accounts + groups (migration 0026) ───────────────────────────────

-- name: ListServiceAccounts :many
SELECT id, name, display_name, project_id, metadata, spec, created_at, updated_at FROM service_accounts ORDER BY name;

-- name: GetServiceAccount :one
SELECT id, name, display_name, project_id, metadata, spec, created_at, updated_at FROM service_accounts WHERE id = $1;

-- name: UpsertServiceAccount :exec
INSERT INTO service_accounts (id, name, display_name, project_id, metadata, spec, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, NOW())
ON CONFLICT (id) DO UPDATE SET
    name         = EXCLUDED.name,
    display_name = EXCLUDED.display_name,
    project_id   = EXCLUDED.project_id,
    metadata     = EXCLUDED.metadata,
    spec         = EXCLUDED.spec,
    updated_at   = NOW();

-- name: DeleteServiceAccount :exec
DELETE FROM service_accounts WHERE id = $1;

-- name: ListGroups :many
SELECT id, name, display_name, metadata, spec, created_at, updated_at FROM groups ORDER BY name;

-- name: GetGroup :one
SELECT id, name, display_name, metadata, spec, created_at, updated_at FROM groups WHERE id = $1;

-- name: UpsertGroup :exec
INSERT INTO groups (id, name, display_name, metadata, spec, updated_at)
VALUES ($1, $2, $3, $4, $5, NOW())
ON CONFLICT (id) DO UPDATE SET
    name         = EXCLUDED.name,
    display_name = EXCLUDED.display_name,
    metadata     = EXCLUDED.metadata,
    spec         = EXCLUDED.spec,
    updated_at   = NOW();

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

-- ── roles + bindings (migration 0027) ────────────────────────────────────────

-- name: ListRoles :many
SELECT id, name, display_name, metadata, spec, created_at, updated_at FROM roles ORDER BY name;

-- name: GetRole :one
SELECT id, name, display_name, metadata, spec, created_at, updated_at FROM roles WHERE id = $1;

-- name: UpsertRole :exec
INSERT INTO roles (id, name, display_name, metadata, spec, updated_at)
VALUES ($1, $2, $3, $4, $5, NOW())
ON CONFLICT (id) DO UPDATE SET
    name         = EXCLUDED.name,
    display_name = EXCLUDED.display_name,
    metadata     = EXCLUDED.metadata,
    spec         = EXCLUDED.spec,
    updated_at   = NOW();

-- name: DeleteRole :exec
DELETE FROM roles WHERE id = $1;

-- name: ListRoleBindings :many
SELECT id, name, display_name, role_id, scope_kind, scope_id, metadata, spec, created_at, updated_at
FROM role_bindings ORDER BY name;

-- name: GetRoleBinding :one
SELECT id, name, display_name, role_id, scope_kind, scope_id, metadata, spec, created_at, updated_at
FROM role_bindings WHERE id = $1;

-- name: UpsertRoleBinding :exec
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
    updated_at   = NOW();

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

-- name: ListPolicyBindings :many
SELECT id, name, display_name, project_id, policy_id, priority, metadata, spec, created_at, updated_at
FROM policy_bindings ORDER BY name;

-- name: GetPolicyBinding :one
SELECT id, name, display_name, project_id, policy_id, priority, metadata, spec, created_at, updated_at
FROM policy_bindings WHERE id = $1;

-- name: UpsertPolicyBinding :exec
INSERT INTO policy_bindings (id, name, display_name, project_id, policy_id, priority, metadata, spec, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
ON CONFLICT (id) DO UPDATE SET
    name         = EXCLUDED.name,
    display_name = EXCLUDED.display_name,
    project_id   = EXCLUDED.project_id,
    policy_id    = EXCLUDED.policy_id,
    priority     = EXCLUDED.priority,
    metadata     = EXCLUDED.metadata,
    spec         = EXCLUDED.spec,
    updated_at   = NOW();

-- name: DeletePolicyBinding :exec
DELETE FROM policy_bindings WHERE id = $1;

-- name: ListPolicyBindingSubjects :many
SELECT binding_id, kind, subject_id, subject_name, position
FROM policy_binding_subjects ORDER BY binding_id, position;

-- name: GetPolicyBindingSubjects :many
SELECT binding_id, kind, subject_id, subject_name, position
FROM policy_binding_subjects WHERE binding_id = $1 ORDER BY position;

-- name: DeletePolicyBindingSubjects :exec
DELETE FROM policy_binding_subjects WHERE binding_id = $1;

-- name: InsertPolicyBindingSubject :exec
INSERT INTO policy_binding_subjects
    (binding_id, kind, subject_id, subject_name, subject_user_id, subject_sa_id, position)
VALUES ($1, $2, $3, $4, $5, $6, $7);
