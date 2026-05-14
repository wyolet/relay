-- 000012: sectioned settings table.
--
-- One row per known section (proxy-mode, branding, limits, ...). Value
-- is opaque JSONB; the typed Go struct for each section lives in
-- app/settings and is validated server-side on PUT.

CREATE TABLE settings (
    section    TEXT PRIMARY KEY,
    value      JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Settings changes ride the same catalog_events channel so the
-- in-memory cache refreshes on every pod within the debounce window.
-- Payload kind is "settings" and rowid is the section name (not a UUID).
CREATE OR REPLACE FUNCTION settings_notify() RETURNS trigger AS $$
DECLARE
    op  TEXT;
    sec TEXT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        op := 'delete';
        sec := OLD.section;
    ELSE
        op := 'upsert';
        sec := NEW.section;
    END IF;
    PERFORM pg_notify('catalog_events', 'settings:' || op || ':' || sec);
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    ELSE
        RETURN NEW;
    END IF;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER settings_notify_trg AFTER INSERT OR UPDATE OR DELETE ON settings
    FOR EACH ROW EXECUTE FUNCTION settings_notify();
