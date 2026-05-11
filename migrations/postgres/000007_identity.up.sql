-- 000007: identity refactor.
--
-- PK becomes the immutable UUIDv7 `id`. The user-visible `name` is now a
-- DNS-label slug, unique-per-table but mutable. `display_name` is free text.
--
-- DESTRUCTIVE: this migration TRUNCATEs every catalog table. Relay is pre-prod;
-- no data is worth migrating. On next pod start the YAML loader re-seeds with
-- fresh ids. JSONB cross-references (spec.secrets[], spec.provider, etc.) are
-- written by app code as ids on re-seed; no in-place rewrite is performed.

TRUNCATE TABLE providers, policies, secrets, models, routes, rate_limits, relay_keys RESTART IDENTITY;

-- Helper applied to every table below: add id+display_name, swap PK.
ALTER TABLE providers   DROP CONSTRAINT providers_pkey,
                        ADD COLUMN id TEXT NOT NULL,
                        ADD COLUMN display_name TEXT NOT NULL DEFAULT '',
                        ADD PRIMARY KEY (id),
                        ADD CONSTRAINT providers_name_unique UNIQUE (name);

-- policies inherited the legacy "pools_pkey" constraint name from migration
-- 000004 (RENAME TABLE preserves constraint names). Drop both possible names.
ALTER TABLE policies    DROP CONSTRAINT IF EXISTS pools_pkey,
                        DROP CONSTRAINT IF EXISTS policies_pkey,
                        ADD COLUMN id TEXT NOT NULL,
                        ADD COLUMN display_name TEXT NOT NULL DEFAULT '',
                        ADD PRIMARY KEY (id),
                        ADD CONSTRAINT policies_name_unique UNIQUE (name);

ALTER TABLE secrets     DROP CONSTRAINT secrets_pkey,
                        ADD COLUMN id TEXT NOT NULL,
                        ADD COLUMN display_name TEXT NOT NULL DEFAULT '',
                        ADD PRIMARY KEY (id),
                        ADD CONSTRAINT secrets_name_unique UNIQUE (name);

ALTER TABLE models      DROP CONSTRAINT models_pkey,
                        ADD COLUMN id TEXT NOT NULL,
                        ADD COLUMN display_name TEXT NOT NULL DEFAULT '',
                        ADD PRIMARY KEY (id),
                        ADD CONSTRAINT models_name_unique UNIQUE (name);

ALTER TABLE routes      DROP CONSTRAINT routes_pkey,
                        ADD COLUMN id TEXT NOT NULL,
                        ADD COLUMN display_name TEXT NOT NULL DEFAULT '',
                        ADD PRIMARY KEY (id),
                        ADD CONSTRAINT routes_name_unique UNIQUE (name);

ALTER TABLE rate_limits DROP CONSTRAINT rate_limits_pkey,
                        ADD COLUMN id TEXT NOT NULL,
                        ADD COLUMN display_name TEXT NOT NULL DEFAULT '',
                        ADD PRIMARY KEY (id),
                        ADD CONSTRAINT rate_limits_name_unique UNIQUE (name);

ALTER TABLE relay_keys  DROP CONSTRAINT relay_keys_pkey,
                        ADD COLUMN id TEXT NOT NULL,
                        ADD COLUMN display_name TEXT NOT NULL DEFAULT '',
                        ADD PRIMARY KEY (id),
                        ADD CONSTRAINT relay_keys_name_unique UNIQUE (name);

-- passthrough_config is a singleton; no id/slug treatment.
