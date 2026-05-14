-- Reverse 000016: strip the backfilled policyId from every HostKey row.
UPDATE secrets
SET spec = spec - 'policyId'
WHERE spec ? 'policyId';
