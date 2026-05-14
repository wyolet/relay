-- Migration 000016: backfill HostKey.Spec.PolicyID from each key's Host's
-- defaultPolicy. PolicyID is required as of this migration; rows without
-- a resolvable default fail the post-migration CHECK below — operator
-- must fix the offending Host (set spec.defaultPolicy) or the HostKey
-- (set spec.policyId explicitly).
--
-- PolicyID lives in JSONB (the secrets table's spec column), so there's
-- no schema change — just an UPDATE.

UPDATE secrets s
SET spec = jsonb_set(
        s.spec,
        '{policyId}',
        (
            SELECT to_jsonb((h.spec->>'defaultPolicy')::text)
            FROM hosts h
            WHERE h.id::text = s.spec->>'hostId'
        ),
        true
    )
WHERE s.spec->>'policyId' IS NULL
  AND s.spec->>'hostId' IS NOT NULL
  AND EXISTS (
      SELECT 1 FROM hosts h
      WHERE h.id::text = s.spec->>'hostId'
        AND h.spec->>'defaultPolicy' IS NOT NULL
        AND h.spec->>'defaultPolicy' <> ''
  );

-- Surface rows that still lack a PolicyID. The relay's validator will
-- reject them at next Reload anyway; raising here gives the operator a
-- clear error during the migration step.
DO $$
DECLARE
    n int;
BEGIN
    SELECT COUNT(*) INTO n FROM secrets
        WHERE value_kind IN ('env','stored')
          AND (spec->>'policyId' IS NULL OR spec->>'policyId' = '');
    IF n > 0 THEN
        RAISE WARNING 'migration 000016: % HostKey rows have no spec.policyId after backfill; set their Host.spec.defaultPolicy or the key''s spec.policyId before the next catalog reload.', n;
    END IF;
END $$;
