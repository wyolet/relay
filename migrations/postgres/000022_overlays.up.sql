-- 000022: catalog overlays.
--
-- An overlay is a user-owned sparse spec patch on top of a catalog
-- TEMPLATE row (the pristine, re-seedable base). The merge to an
-- EFFECTIVE row happens at catalog load time (app/catalog), never in
-- storage — re-seeding replaces templates and is completely
-- overlay-unaware, so user customizations survive catalog upgrades.
-- See docs/overlays.md.
--
-- kind is the target resource kind ("model" in v1); resource_id the
-- target row id. No FK: kinds are heterogeneous and an orphan overlay
-- (target deleted) is inert — it simply never merges.

CREATE TABLE IF NOT EXISTS overlays (
    kind        TEXT  NOT NULL,
    resource_id TEXT  NOT NULL,
    patch       JSONB NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (kind, resource_id)
);

-- NOTIFY: same catalog_events channel as 000011, but overlays have a
-- composite key, so a dedicated trigger function encodes both halves in
-- the id slot: "overlay:<op>:<kind>|<resource_id>".
CREATE OR REPLACE FUNCTION overlay_notify() RETURNS trigger AS $$
DECLARE
    op  TEXT;
    ref TEXT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        op := 'delete';
        ref := OLD.kind || '|' || OLD.resource_id;
    ELSE
        op := 'upsert';
        ref := NEW.kind || '|' || NEW.resource_id;
    END IF;
    PERFORM pg_notify('catalog_events', 'overlay:' || op || ':' || ref);
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    ELSE
        RETURN NEW;
    END IF;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER overlays_notify AFTER INSERT OR UPDATE OR DELETE ON overlays
    FOR EACH ROW EXECUTE FUNCTION overlay_notify();
