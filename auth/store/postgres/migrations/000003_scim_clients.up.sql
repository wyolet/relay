-- One row per SCIM client: a provisioning source and the hash of its bearer token, never the token. The name is the external_source stamped on the users and groups the client creates.
CREATE TABLE IF NOT EXISTS scim_clients (
    name         TEXT PRIMARY KEY,
    token_hash   TEXT NOT NULL UNIQUE,
    token_prefix TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);
