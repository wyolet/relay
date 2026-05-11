-- name: ListProviders :many
SELECT id, name, display_name, metadata, spec FROM providers ORDER BY name;

-- name: ListPolicies :many
SELECT id, name, display_name, metadata, spec FROM policies ORDER BY name;

-- name: ListSecrets :many
SELECT id, name, display_name, metadata, spec, value_kind, value_from_env, value_ciphertext, value_nonce FROM secrets ORDER BY name;

-- name: ListModels :many
SELECT id, name, display_name, metadata, spec FROM models ORDER BY name;

-- name: ListRoutes :many
SELECT id, name, display_name, metadata, spec FROM routes ORDER BY name;

-- name: ListRateLimits :many
SELECT id, name, display_name, metadata, spec FROM rate_limits ORDER BY name;

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
INSERT INTO policies (id, name, display_name, metadata, spec, updated_at)
VALUES ($1, $2, $3, $4, $5, NOW())
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name,
    display_name = EXCLUDED.display_name,
    metadata = EXCLUDED.metadata,
    spec = EXCLUDED.spec,
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
    SET name           = EXCLUDED.name,
        display_name   = EXCLUDED.display_name,
        value_kind     = 'env',
        value_from_env = EXCLUDED.value_from_env,
        value_ciphertext = NULL,
        value_nonce      = NULL,
        metadata         = EXCLUDED.metadata,
        spec             = EXCLUDED.spec,
        updated_at       = NOW()
RETURNING id, name, display_name, value_kind, value_from_env, value_ciphertext, value_nonce, metadata, spec;

-- name: InsertSecretStored :one
INSERT INTO secrets (id, name, display_name, value_kind, value_ciphertext, value_nonce, metadata, spec)
VALUES ($1, $2, $3, 'stored', $4, $5, $6, $7)
ON CONFLICT (id) DO UPDATE
    SET name             = EXCLUDED.name,
        display_name     = EXCLUDED.display_name,
        value_kind       = 'stored',
        value_from_env   = NULL,
        value_ciphertext = EXCLUDED.value_ciphertext,
        value_nonce      = EXCLUDED.value_nonce,
        metadata         = EXCLUDED.metadata,
        spec             = EXCLUDED.spec,
        updated_at       = NOW()
RETURNING id, name, display_name, value_kind, value_from_env, value_ciphertext, value_nonce, metadata, spec;

-- name: UpdateSecretEnv :one
UPDATE secrets
SET value_kind       = 'env',
    value_from_env   = $2,
    value_ciphertext = NULL,
    value_nonce      = NULL,
    updated_at       = NOW()
WHERE id = $1
RETURNING id, name, display_name, value_kind, value_from_env, value_ciphertext, value_nonce, metadata, spec;

-- name: UpdateSecretStored :one
UPDATE secrets
SET value_kind       = 'stored',
    value_from_env   = NULL,
    value_ciphertext = $2,
    value_nonce      = $3,
    updated_at       = NOW()
WHERE id = $1
RETURNING id, name, display_name, value_kind, value_from_env, value_ciphertext, value_nonce, metadata, spec;

-- name: DeleteSecret :exec
DELETE FROM secrets WHERE id = $1;

-- name: UpsertModel :exec
INSERT INTO models (id, name, display_name, metadata, spec, updated_at)
VALUES ($1, $2, $3, $4, $5, NOW())
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name,
    display_name = EXCLUDED.display_name,
    metadata = EXCLUDED.metadata,
    spec = EXCLUDED.spec,
    updated_at = NOW();

-- name: UpsertRoute :exec
INSERT INTO routes (id, name, display_name, metadata, spec, updated_at)
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

-- name: DeleteRoute :exec
DELETE FROM routes WHERE id = $1;

-- name: DeleteRateLimit :exec
DELETE FROM rate_limits WHERE id = $1;

-- name: ListRelayKeys :many
SELECT id, name, display_name, key_hash, metadata, spec FROM relay_keys ORDER BY name;

-- name: UpsertRelayKey :exec
INSERT INTO relay_keys (id, name, display_name, key_hash, metadata, spec, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, NOW())
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name,
    display_name = EXCLUDED.display_name,
    key_hash   = EXCLUDED.key_hash,
    metadata   = EXCLUDED.metadata,
    spec       = EXCLUDED.spec,
    updated_at = NOW();

-- name: DeleteRelayKey :exec
DELETE FROM relay_keys WHERE id = $1;

-- name: GetPassthrough :one
SELECT name, spec FROM passthrough_config WHERE name = $1;

-- name: UpsertPassthrough :exec
INSERT INTO passthrough_config (name, spec, updated_at)
VALUES ($1, $2, NOW())
ON CONFLICT (name) DO UPDATE SET
    spec       = EXCLUDED.spec,
    updated_at = NOW();
