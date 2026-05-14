-- 000013: policies.models — wildcard ref-string grants alongside the
-- existing policy_models junction.
--
-- Pattern grants ("anthropic", "anthropic/claude-opus-4-7@bedrock", etc.)
-- can't sit in the policy_models junction because they're literal strings
-- with optional wildcards, not Model UUIDs. We add a JSONB column on
-- policies that stores the verbatim string array; the snapshot expands
-- it at build time against the live catalog.
--
-- This migration is ADDITIVE — the policy_models junction stays for
-- legacy literal-ID grants. A future migration will deprecate it once
-- all writers have switched.

ALTER TABLE policies
    ADD COLUMN IF NOT EXISTS models JSONB NOT NULL DEFAULT '[]'::jsonb;

-- NOTIFY trigger: emit a 'policy' channel notification when models
-- changes, same as updates to spec/rate_limit_id today. The existing
-- policies_notify trigger already fires on UPDATE — no new trigger
-- needed, but document the new column belongs to the same row.
