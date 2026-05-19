-- 000017: drop the routes and passthrough_config tables.
--
-- routes was the v0 model-routing primitive, superseded by Policy +
-- RelayKey grants (CLAUDE.md: "Route entity is deferred"). It hasn't
-- been read or written by any code path since the app/ arch cutover.
--
-- passthrough_config was a singleton-shaped table for the legacy
-- passthrough toggle; the equivalent is now sectioned settings (see
-- 000012). Nothing reads or writes it.
--
-- No-op in production: both tables have been empty since the cutover.

DROP TABLE IF EXISTS routes;
DROP TABLE IF EXISTS passthrough_config;
