-- Migration 000015: HostKey owner refactor.
--
-- Pre: metadata.owner = {kind: host, id: <host id>}. The Host was conflated
-- with the *creator* of the key.
--
-- Post: metadata.owner = {kind: system} for migrated rows (no user existed
-- at the time of creation; admin UI can re-stamp on next update). The
-- target Host moves into spec.hostId.
--
-- Existing rows are backfilled in-place; no schema change is needed because
-- both fields live in JSONB. New rows go through the domain validator
-- which enforces the new shape.

UPDATE secrets
SET spec = jsonb_set(
        spec,
        '{hostId}',
        to_jsonb(COALESCE(metadata->'owner'->>'id', '')::text),
        true
    ),
    metadata = jsonb_set(
        metadata,
        '{owner}',
        '{"kind":"system"}'::jsonb,
        true
    );
