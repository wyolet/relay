-- Restore attachments table (rollback of 000003).
CREATE TABLE IF NOT EXISTS attachments (
    parent_kind     TEXT NOT NULL,
    parent_name     TEXT NOT NULL,
    ratelimit_name  TEXT NOT NULL,
    meter           TEXT NOT NULL,
    PRIMARY KEY (parent_kind, parent_name, ratelimit_name, meter)
);

CREATE INDEX IF NOT EXISTS attachments_parent_idx ON attachments (parent_kind, parent_name);
