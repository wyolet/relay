-- 000008 down: remove owner field from metadata on all catalog tables.
-- Note: spec.source / spec.window / spec.strategy are NOT restored — they were
-- removed by the Go types and cannot be meaningfully reconstructed.

UPDATE rate_limits
SET metadata = metadata - 'owner' - 'description';

UPDATE providers  SET metadata = metadata - 'owner';
UPDATE secrets    SET metadata = metadata - 'owner';
UPDATE policies   SET metadata = metadata - 'owner';
UPDATE routes     SET metadata = metadata - 'owner';
UPDATE relay_keys SET metadata = metadata - 'owner';
UPDATE models     SET metadata = metadata - 'owner';
