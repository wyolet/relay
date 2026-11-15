-- 000043: when each relay key and host key last served a request, to the minute.
--
-- Tables of their own, with no catalog trigger: every pod writes them about once a minute, and a write to relay_keys or secrets would notify every pod to rebuild its snapshot. Pods write concurrently; each keeps the later time.

CREATE TABLE IF NOT EXISTS relay_key_last_used (
    key_id       TEXT PRIMARY KEY REFERENCES relay_keys(id) ON DELETE CASCADE,
    last_used_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS host_key_last_used (
    host_key_id  TEXT PRIMARY KEY REFERENCES secrets(id) ON DELETE CASCADE,
    last_used_at TIMESTAMPTZ NOT NULL
);
