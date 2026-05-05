-- name: ListProviders :many
SELECT name, metadata, spec FROM providers ORDER BY name;

-- name: ListPools :many
SELECT name, metadata, spec FROM pools ORDER BY name;

-- name: ListSecrets :many
SELECT name, metadata, spec FROM secrets ORDER BY name;

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

-- name: UpsertPool :exec
INSERT INTO pools (name, metadata, spec, updated_at)
VALUES ($1, $2, $3, NOW())
ON CONFLICT (name) DO UPDATE SET metadata = EXCLUDED.metadata, spec = EXCLUDED.spec, updated_at = NOW();

-- name: UpsertSecret :exec
INSERT INTO secrets (name, metadata, spec, updated_at)
VALUES ($1, $2, $3, NOW())
ON CONFLICT (name) DO UPDATE SET metadata = EXCLUDED.metadata, spec = EXCLUDED.spec, updated_at = NOW();

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
