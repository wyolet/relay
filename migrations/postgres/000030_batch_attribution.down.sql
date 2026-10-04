DROP INDEX IF EXISTS batches_project_idx;

ALTER TABLE batches DROP COLUMN IF EXISTS project_id;
ALTER TABLE batches DROP COLUMN IF EXISTS team_id;
ALTER TABLE batches DROP COLUMN IF EXISTS principal_kind;
ALTER TABLE batches DROP COLUMN IF EXISTS principal_id;
ALTER TABLE batches DROP COLUMN IF EXISTS credential_kind;
ALTER TABLE batches DROP COLUMN IF EXISTS credential_id;
