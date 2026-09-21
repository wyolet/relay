-- A batch's attribution is fixed at submit: the principal and tenancy that
-- authorised it, not whatever the credential resolves to when an item finally
-- runs. Ids only — the slugs are resolved from the snapshot at emit, like the
-- policy name already is.
ALTER TABLE batches ADD COLUMN IF NOT EXISTS project_id      TEXT NOT NULL DEFAULT '';
ALTER TABLE batches ADD COLUMN IF NOT EXISTS team_id         TEXT NOT NULL DEFAULT '';
ALTER TABLE batches ADD COLUMN IF NOT EXISTS principal_kind  TEXT NOT NULL DEFAULT '';
ALTER TABLE batches ADD COLUMN IF NOT EXISTS principal_id    TEXT NOT NULL DEFAULT '';
ALTER TABLE batches ADD COLUMN IF NOT EXISTS credential_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE batches ADD COLUMN IF NOT EXISTS credential_id   TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS batches_project_idx ON batches (project_id, created_at DESC);
