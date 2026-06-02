-- 000020: host_bindings entity — the first-class (Model, Host) join.
--
-- Promotes the binding out of models.spec->'hosts' JSONB into its own row so
-- pricing and routing can reference a real binding, and one Model can be
-- bound by many Hosts (incl. aggregators re-serving it).
--
-- model_id / host_id / pricing_id are real columns (not just JSONB) so FK
-- cascade handles deletes and the snapshot builder joins without parsing
-- JSONB — same rationale as pricings.host_id. The rest (adapter,
-- upstreamName, enabled, the snapshots subset) lives in the JSONB spec; the
-- snapshots subset is a list of names (a filter, not FK refs), so it stays
-- in spec rather than getting a junction table.
--
-- UNIQUE (model_id, host_id): one binding per (model, host) pair.
-- pricing_id ON DELETE SET NULL: dropping a Pricing unprices the binding, it
-- does not delete it. model_id/host_id CASCADE: the binding is meaningless
-- without both endpoints.

CREATE TABLE IF NOT EXISTS host_bindings (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL DEFAULT '',
    model_id     TEXT NOT NULL REFERENCES models(id)   ON DELETE CASCADE,
    host_id      TEXT NOT NULL REFERENCES hosts(id)     ON DELETE CASCADE,
    pricing_id   TEXT          REFERENCES pricings(id)  ON DELETE SET NULL,
    metadata     JSONB NOT NULL,
    spec         JSONB NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (model_id, host_id)
);
CREATE INDEX IF NOT EXISTS host_bindings_model_idx   ON host_bindings (model_id);
CREATE INDEX IF NOT EXISTS host_bindings_host_idx    ON host_bindings (host_id);
CREATE INDEX IF NOT EXISTS host_bindings_pricing_idx ON host_bindings (pricing_id);

CREATE TRIGGER host_bindings_notify AFTER INSERT OR UPDATE OR DELETE ON host_bindings
    FOR EACH ROW EXECUTE FUNCTION catalog_notify('hostbinding');
