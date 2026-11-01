-- 000036: drop the pre-v0.20 policy links. A policy now reaches keys through its pools and credentials and is capped by its rate limits and budgets (000035); a host key names its account; keys and service accounts hold no policy, a policy binding binds their principal.
--
-- Moving data to the new shape is manual, so nothing is converted: the legacy model-id and host-key junctions, the single rate-limit column, and the old fields inside the spec documents are removed.

DROP TABLE IF EXISTS policy_models;
DROP TABLE IF EXISTS policy_host_keys;
ALTER TABLE policies DROP COLUMN IF EXISTS rate_limit_id;

UPDATE policies SET spec = spec - 'modelIds' - 'hostKeyIds' - 'rateLimitId' - 'rlBindings' - 'keySelection'
WHERE spec ?| ARRAY['modelIds', 'hostKeyIds', 'rateLimitId', 'rlBindings', 'keySelection'];
UPDATE relay_keys SET spec = spec - 'policyId' WHERE spec ? 'policyId';
UPDATE service_accounts SET spec = spec - 'policyId' WHERE spec ? 'policyId';
UPDATE secrets SET spec = spec - 'policyId' WHERE spec ? 'policyId';
