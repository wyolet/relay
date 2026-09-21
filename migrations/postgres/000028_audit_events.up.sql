-- 000028: admin audit log.
--
-- One row per audited control-plane request: who, what action, on which
-- resource, allowed or denied, and which JSON paths a write touched. Values
-- are never stored — changed_fields holds paths only.
--
-- No NOTIFY trigger: audit rows are append-only history, not catalog state,
-- so no snapshot needs to react to them.

CREATE TABLE IF NOT EXISTS audit_events (
    id             TEXT PRIMARY KEY,
    ts             TIMESTAMPTZ NOT NULL,
    actor_kind     TEXT NOT NULL,
    actor_id       TEXT,
    actor_name     TEXT,
    session_id     TEXT,
    ip             TEXT,
    action         TEXT NOT NULL,
    resource_kind  TEXT NOT NULL,
    resource_id    TEXT,
    resource_name  TEXT,
    owner_kind     TEXT,
    owner_id       TEXT,
    scope          TEXT[] NOT NULL DEFAULT '{}',
    status         TEXT NOT NULL,
    code           INT NOT NULL,
    request_id     TEXT,
    method         TEXT,
    path           TEXT,
    changed_fields TEXT[]
);

CREATE INDEX IF NOT EXISTS audit_events_ts_idx ON audit_events (ts DESC);
CREATE INDEX IF NOT EXISTS audit_events_actor_idx ON audit_events (actor_id, ts DESC);
CREATE INDEX IF NOT EXISTS audit_events_resource_idx ON audit_events (resource_kind, resource_id, ts DESC);
CREATE INDEX IF NOT EXISTS audit_events_scope_idx ON audit_events USING GIN (scope);
