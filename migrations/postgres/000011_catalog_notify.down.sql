DROP TRIGGER IF EXISTS pricing_models_notify   ON pricing_models;
DROP TRIGGER IF EXISTS policy_host_keys_notify ON policy_host_keys;
DROP TRIGGER IF EXISTS policy_models_notify    ON policy_models;

DROP TRIGGER IF EXISTS relay_keys_notify  ON relay_keys;
DROP TRIGGER IF EXISTS pricings_notify    ON pricings;
DROP TRIGGER IF EXISTS policies_notify    ON policies;
DROP TRIGGER IF EXISTS rate_limits_notify ON rate_limits;
DROP TRIGGER IF EXISTS secrets_notify     ON secrets;
DROP TRIGGER IF EXISTS models_notify      ON models;
DROP TRIGGER IF EXISTS hosts_notify       ON hosts;
DROP TRIGGER IF EXISTS providers_notify   ON providers;

DROP FUNCTION IF EXISTS catalog_notify_junction();
DROP FUNCTION IF EXISTS catalog_notify();
