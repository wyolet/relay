-- 000011: catalog change-notification triggers.
--
-- Every catalog mutation emits a NOTIFY on channel `catalog_events` with
-- a small text payload "kind:op:id". The data-plane listener parses this,
-- fetches the affected row, and feeds it through the COW reconciler.
--
-- Junction tables emit with the *parent* kind/id so the listener re-fetches
-- the parent (whose materialised cross-refs depend on the junction).
--
-- Payload format is intentionally tiny (<100 bytes) to stay comfortably
-- inside PG's 8KB NOTIFY limit and parse fast.

CREATE OR REPLACE FUNCTION catalog_notify() RETURNS trigger AS $$
DECLARE
    payload TEXT;
    kind    TEXT := TG_ARGV[0];
    op      TEXT;
    rowid   TEXT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        op := 'delete';
        rowid := OLD.id;
    ELSE
        op := 'upsert';
        rowid := NEW.id;
    END IF;
    payload := kind || ':' || op || ':' || rowid;
    PERFORM pg_notify('catalog_events', payload);
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    ELSE
        RETURN NEW;
    END IF;
END;
$$ LANGUAGE plpgsql;

-- Junction tables emit using the parent (policy / pricing) so the
-- listener re-fetches the parent row whose cross-refs changed.
CREATE OR REPLACE FUNCTION catalog_notify_junction() RETURNS trigger AS $$
DECLARE
    parent_kind  TEXT := TG_ARGV[0];
    parent_col   TEXT := TG_ARGV[1];
    payload      TEXT;
    parent_id    TEXT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        EXECUTE format('SELECT ($1).%I', parent_col) INTO parent_id USING OLD;
    ELSE
        EXECUTE format('SELECT ($1).%I', parent_col) INTO parent_id USING NEW;
    END IF;
    payload := parent_kind || ':upsert:' || parent_id;
    PERFORM pg_notify('catalog_events', payload);
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    ELSE
        RETURN NEW;
    END IF;
END;
$$ LANGUAGE plpgsql;

-- Catalog tables with id PK.
CREATE TRIGGER providers_notify   AFTER INSERT OR UPDATE OR DELETE ON providers
    FOR EACH ROW EXECUTE FUNCTION catalog_notify('provider');
CREATE TRIGGER hosts_notify       AFTER INSERT OR UPDATE OR DELETE ON hosts
    FOR EACH ROW EXECUTE FUNCTION catalog_notify('host');
CREATE TRIGGER models_notify      AFTER INSERT OR UPDATE OR DELETE ON models
    FOR EACH ROW EXECUTE FUNCTION catalog_notify('model');
CREATE TRIGGER secrets_notify     AFTER INSERT OR UPDATE OR DELETE ON secrets
    FOR EACH ROW EXECUTE FUNCTION catalog_notify('hostkey');
CREATE TRIGGER rate_limits_notify AFTER INSERT OR UPDATE OR DELETE ON rate_limits
    FOR EACH ROW EXECUTE FUNCTION catalog_notify('ratelimit');
CREATE TRIGGER policies_notify    AFTER INSERT OR UPDATE OR DELETE ON policies
    FOR EACH ROW EXECUTE FUNCTION catalog_notify('policy');
CREATE TRIGGER pricings_notify    AFTER INSERT OR UPDATE OR DELETE ON pricings
    FOR EACH ROW EXECUTE FUNCTION catalog_notify('pricing');
CREATE TRIGGER relay_keys_notify  AFTER INSERT OR UPDATE OR DELETE ON relay_keys
    FOR EACH ROW EXECUTE FUNCTION catalog_notify('relaykey');

-- Junctions: emit as the parent kind so the parent re-fetch picks up the
-- new junction state.
CREATE TRIGGER policy_models_notify    AFTER INSERT OR UPDATE OR DELETE ON policy_models
    FOR EACH ROW EXECUTE FUNCTION catalog_notify_junction('policy', 'policy_id');
CREATE TRIGGER policy_host_keys_notify AFTER INSERT OR UPDATE OR DELETE ON policy_host_keys
    FOR EACH ROW EXECUTE FUNCTION catalog_notify_junction('policy', 'policy_id');
CREATE TRIGGER pricing_models_notify   AFTER INSERT OR UPDATE OR DELETE ON pricing_models
    FOR EACH ROW EXECUTE FUNCTION catalog_notify_junction('pricing', 'pricing_id');
