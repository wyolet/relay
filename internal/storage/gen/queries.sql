-- name: ListProviders :many
SELECT name, metadata, spec FROM providers ORDER BY name;

-- name: ListPolicies :many
SELECT name, metadata, spec FROM policies ORDER BY name;

-- name: ListSecrets :many
SELECT name, metadata, spec, value_kind, value_from_env, value_ciphertext, value_nonce FROM secrets ORDER BY name;

-- name: ListModels :many
SELECT name, metadata, spec FROM models ORDER BY name;

-- name: ListRoutes :many
SELECT name, metadata, spec FROM routes ORDER BY name;

-- name: ListRateLimits :many
SELECT name, metadata, spec FROM rate_limits ORDER BY name;

-- name: ListAttachments :many
SELECT parent_kind, parent_name, ratelimit_name, meter FROM attachments ORDER BY parent_kind, parent_name;

-- name: UpsertProvider :exec
INSERT INTO providers (name, metadata, spec, updated_at)
VALUES ($1, $2, $3, NOW())
ON CONFLICT (name) DO UPDATE SET metadata = EXCLUDED.metadata, spec = EXCLUDED.spec, updated_at = NOW();

-- name: UpsertPolicy :exec
INSERT INTO policies (name, metadata, spec, updated_at)
VALUES ($1, $2, $3, NOW())
ON CONFLICT (name) DO UPDATE SET metadata = EXCLUDED.metadata, spec = EXCLUDED.spec, updated_at = NOW();

-- UpsertSecret is kept for the seed CLI (YAML-import path). Deprecated for new code; use InsertSecretEnv / InsertSecretStored.
-- name: UpsertSecret :exec
INSERT INTO secrets (name, metadata, spec, updated_at)
VALUES ($1, $2, $3, NOW())
ON CONFLICT (name) DO UPDATE SET metadata = EXCLUDED.metadata, spec = EXCLUDED.spec, updated_at = NOW();

-- name: InsertSecretEnv :one
INSERT INTO secrets (name, value_kind, value_from_env, metadata, spec)
VALUES ($1, 'env', $2, $3, $4)
ON CONFLICT (name) DO UPDATE
    SET value_kind     = 'env',
        value_from_env = EXCLUDED.value_from_env,
        value_ciphertext = NULL,
        value_nonce      = NULL,
        metadata         = EXCLUDED.metadata,
        spec             = EXCLUDED.spec,
        updated_at       = NOW()
RETURNING name, value_kind, value_from_env, value_ciphertext, value_nonce, metadata, spec;

-- name: InsertSecretStored :one
INSERT INTO secrets (name, value_kind, value_ciphertext, value_nonce, metadata, spec)
VALUES ($1, 'stored', $2, $3, $4, $5)
ON CONFLICT (name) DO UPDATE
    SET value_kind       = 'stored',
        value_from_env   = NULL,
        value_ciphertext = EXCLUDED.value_ciphertext,
        value_nonce      = EXCLUDED.value_nonce,
        metadata         = EXCLUDED.metadata,
        spec             = EXCLUDED.spec,
        updated_at       = NOW()
RETURNING name, value_kind, value_from_env, value_ciphertext, value_nonce, metadata, spec;

-- name: UpdateSecretEnv :one
UPDATE secrets
SET value_kind       = 'env',
    value_from_env   = $2,
    value_ciphertext = NULL,
    value_nonce      = NULL,
    updated_at       = NOW()
WHERE name = $1
RETURNING name, value_kind, value_from_env, value_ciphertext, value_nonce, metadata, spec;

-- name: UpdateSecretStored :one
UPDATE secrets
SET value_kind       = 'stored',
    value_from_env   = NULL,
    value_ciphertext = $2,
    value_nonce      = $3,
    updated_at       = NOW()
WHERE name = $1
RETURNING name, value_kind, value_from_env, value_ciphertext, value_nonce, metadata, spec;

-- name: DeleteSecret :exec
DELETE FROM secrets WHERE name = $1;

-- name: UpsertModel :exec
INSERT INTO models (name, metadata, spec, updated_at)
VALUES ($1, $2, $3, NOW())
ON CONFLICT (name) DO UPDATE SET metadata = EXCLUDED.metadata, spec = EXCLUDED.spec, updated_at = NOW();

-- name: UpsertRoute :exec
INSERT INTO routes (name, metadata, spec, updated_at)
VALUES ($1, $2, $3, NOW())
ON CONFLICT (name) DO UPDATE SET metadata = EXCLUDED.metadata, spec = EXCLUDED.spec, updated_at = NOW();

-- name: UpsertRateLimit :exec
INSERT INTO rate_limits (name, metadata, spec, updated_at)
VALUES ($1, $2, $3, NOW())
ON CONFLICT (name) DO UPDATE SET metadata = EXCLUDED.metadata, spec = EXCLUDED.spec, updated_at = NOW();

-- name: UpsertAttachment :exec
INSERT INTO attachments (parent_kind, parent_name, ratelimit_name, meter)
VALUES ($1, $2, $3, $4)
ON CONFLICT DO NOTHING;

-- name: InsertAttachment :one
INSERT INTO attachments (parent_kind, parent_name, ratelimit_name, meter)
VALUES ($1, $2, $3, $4)
ON CONFLICT DO NOTHING
RETURNING parent_kind, parent_name, ratelimit_name, meter;

-- name: ListAttachmentsByParent :many
SELECT parent_kind, parent_name, ratelimit_name, meter
FROM attachments
WHERE parent_kind = $1 AND parent_name = $2
ORDER BY ratelimit_name, meter;

-- name: DeleteAttachmentByCompositeKey :execrows
DELETE FROM attachments
WHERE parent_kind = $1 AND parent_name = $2 AND ratelimit_name = $3 AND meter = $4;

-- name: DeleteProvider :exec
DELETE FROM providers WHERE name = $1;

-- name: DeletePolicy :exec
DELETE FROM policies WHERE name = $1;

-- name: DeleteModel :exec
DELETE FROM models WHERE name = $1;

-- name: DeleteRoute :exec
DELETE FROM routes WHERE name = $1;

-- name: DeleteRateLimit :exec
DELETE FROM rate_limits WHERE name = $1;
