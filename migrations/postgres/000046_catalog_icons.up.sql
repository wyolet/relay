-- 000046: the icon files of the seeded catalog release.
--
-- A catalog entity's icon path "/provider/<name>" names one row. The seed
-- replaces the whole set from the release's icons/ directory, so every pod
-- serves the icons of the catalog in Postgres, not of the tree in its image.

CREATE TABLE catalog_icons (
    name       TEXT PRIMARY KEY,
    body       BYTEA NOT NULL,
    sha256     TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
