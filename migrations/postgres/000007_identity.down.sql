-- Rollback for 000007. Also destructive: TRUNCATEs and reverts PK to name.

TRUNCATE TABLE providers, policies, secrets, models, routes, rate_limits, relay_keys RESTART IDENTITY;

ALTER TABLE providers   DROP CONSTRAINT providers_pkey,
                        DROP CONSTRAINT providers_name_unique,
                        DROP COLUMN id,
                        DROP COLUMN display_name,
                        ADD PRIMARY KEY (name);

ALTER TABLE policies    DROP CONSTRAINT policies_pkey,
                        DROP CONSTRAINT policies_name_unique,
                        DROP COLUMN id,
                        DROP COLUMN display_name,
                        ADD PRIMARY KEY (name);

ALTER TABLE secrets     DROP CONSTRAINT secrets_pkey,
                        DROP CONSTRAINT secrets_name_unique,
                        DROP COLUMN id,
                        DROP COLUMN display_name,
                        ADD PRIMARY KEY (name);

ALTER TABLE models      DROP CONSTRAINT models_pkey,
                        DROP CONSTRAINT models_name_unique,
                        DROP COLUMN id,
                        DROP COLUMN display_name,
                        ADD PRIMARY KEY (name);

ALTER TABLE routes      DROP CONSTRAINT routes_pkey,
                        DROP CONSTRAINT routes_name_unique,
                        DROP COLUMN id,
                        DROP COLUMN display_name,
                        ADD PRIMARY KEY (name);

ALTER TABLE rate_limits DROP CONSTRAINT rate_limits_pkey,
                        DROP CONSTRAINT rate_limits_name_unique,
                        DROP COLUMN id,
                        DROP COLUMN display_name,
                        ADD PRIMARY KEY (name);

ALTER TABLE relay_keys  DROP CONSTRAINT relay_keys_pkey,
                        DROP CONSTRAINT relay_keys_name_unique,
                        DROP COLUMN id,
                        DROP COLUMN display_name,
                        ADD PRIMARY KEY (name);
