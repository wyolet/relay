-- Observed credential state on hostkeys (secrets rows): written by the
-- OAuth refresher (renewed / revoked), cleared when the operator uploads
-- a new value. Desired config stays in spec; this is status.
ALTER TABLE secrets ADD COLUMN status jsonb;
