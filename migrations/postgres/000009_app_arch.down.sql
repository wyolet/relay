ALTER TABLE policies DROP COLUMN IF EXISTS rate_limit_id;
DROP TABLE IF EXISTS policy_host_keys;
DROP TABLE IF EXISTS policy_models;
DROP TABLE IF EXISTS hosts;
