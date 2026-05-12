-- 000008: owner refactor (issue #105).
--
-- 1. RateLimit rows: migrate spec.source → metadata.owner, drop spec-level
--    source/window/strategy/description from the stored JSONB.
-- 2. All other catalog kinds: set a default metadata.owner in the JSONB based
--    on the table (system for providers; user for secrets/policies/routes/relay_keys;
--    provider-keyed for models).
--
-- Forward-only. The Go types no longer have spec.source/window/strategy so the
-- app will not regenerate them; this migration cleans up existing stored rows.
-- Idempotent: jsonb_set on absent keys is a no-op.

-- ── rate_limits ──────────────────────────────────────────────────────────────

-- Step 1: fan out spec-level window/strategy into each rule (in-place JSONB
--         rewrite) so rules are self-contained after the migration.
UPDATE rate_limits
SET spec = (
    SELECT jsonb_set(
        jsonb_set(
            spec,
            '{rules}',
            COALESCE(
                (
                    SELECT jsonb_agg(
                        jsonb_set(
                            jsonb_set(
                                rule_elem,
                                '{window}',
                                COALESCE(
                                    NULLIF(rule_elem->>'window', ''),
                                    spec->>'window',
                                    '"0"'
                                )::jsonb
                            ),
                            '{strategy}',
                            to_jsonb(
                                COALESCE(
                                    NULLIF(rule_elem->>'strategy', ''),
                                    spec->>'strategy',
                                    'token-bucket'
                                )
                            )
                        )
                    )
                    FROM jsonb_array_elements(spec->'rules') AS rule_elem
                ),
                spec->'rules'
            )
        ),
        '{rules}',
        COALESCE(spec->'rules', '[]'::jsonb)
    )
    -- unreachable placeholder; outer expression is the real value
)
WHERE spec ? 'rules' AND jsonb_array_length(spec->'rules') > 0
  AND (spec ? 'window' OR spec ? 'strategy');

-- Step 2: set metadata.owner from spec.source.
UPDATE rate_limits
SET spec = spec
    - 'source'
    - 'window'
    - 'strategy'
    - 'description',
    metadata = jsonb_set(
        jsonb_set(
            metadata,
            '{owner}',
            CASE
                WHEN spec->>'source' = 'system_mirrored'
                    THEN '{"kind":"system"}'::jsonb
                ELSE '{"kind":"user"}'::jsonb
            END
        ),
        '{description}',
        to_jsonb(COALESCE(spec->>'description', ''))
    );

-- ── providers ────────────────────────────────────────────────────────────────
UPDATE providers
SET metadata = jsonb_set(metadata, '{owner}', '{"kind":"system"}'::jsonb)
WHERE NOT (metadata ? 'owner');

-- ── secrets ──────────────────────────────────────────────────────────────────
UPDATE secrets
SET metadata = jsonb_set(metadata, '{owner}', '{"kind":"user"}'::jsonb)
WHERE NOT (metadata ? 'owner');

-- ── policies ─────────────────────────────────────────────────────────────────
UPDATE policies
SET metadata = jsonb_set(metadata, '{owner}', '{"kind":"user"}'::jsonb)
WHERE NOT (metadata ? 'owner');

-- ── routes ───────────────────────────────────────────────────────────────────
UPDATE routes
SET metadata = jsonb_set(metadata, '{owner}', '{"kind":"user"}'::jsonb)
WHERE NOT (metadata ? 'owner');

-- ── relay_keys ───────────────────────────────────────────────────────────────
UPDATE relay_keys
SET metadata = jsonb_set(metadata, '{owner}', '{"kind":"user"}'::jsonb)
WHERE NOT (metadata ? 'owner');

-- ── models ───────────────────────────────────────────────────────────────────
-- Models are owned by their provider. We store kind=provider and id=provider_name
-- (slug) since provider UUIDs require a join and the slug is stable for now.
UPDATE models m
SET metadata = jsonb_set(
    m.metadata,
    '{owner}',
    jsonb_build_object('kind', 'provider', 'id', m.spec->>'provider')
)
WHERE NOT (m.metadata ? 'owner');
