-- Reverse 000035. Policy-binding subjects of the added kinds, and bindings owned by a team or the system, are deleted: the restored checks refuse them. A HostKey's spec.accountId is left in place; older releases ignore it.

DROP INDEX IF EXISTS policy_binding_subjects_one_binding;
DELETE FROM policy_bindings WHERE project_id IS NULL;
DROP INDEX IF EXISTS policy_bindings_team_idx;
ALTER TABLE policy_bindings DROP CONSTRAINT IF EXISTS policy_bindings_one_owner;
ALTER TABLE policy_bindings DROP COLUMN IF EXISTS team_id;
ALTER TABLE policy_bindings ALTER COLUMN project_id SET NOT NULL;
DELETE FROM policy_binding_subjects WHERE kind NOT IN ('user', 'group', 'serviceaccount');
ALTER TABLE policy_binding_subjects DROP CONSTRAINT IF EXISTS policy_binding_subjects_kind_check;
ALTER TABLE policy_binding_subjects ADD CONSTRAINT policy_binding_subjects_kind_check
    CHECK (kind IN ('user', 'group', 'serviceaccount'));
ALTER TABLE policy_binding_subjects
    DROP COLUMN IF EXISTS subject_host_id,
    DROP COLUMN IF EXISTS subject_account_id,
    DROP COLUMN IF EXISTS subject_credential_id,
    DROP COLUMN IF EXISTS subject_team_id,
    DROP COLUMN IF EXISTS subject_project_id;

DROP TABLE IF EXISTS policy_budgets;
DROP TABLE IF EXISTS policy_rate_limits;
DROP TABLE IF EXISTS policy_credentials;
DROP TABLE IF EXISTS policy_pools;
DROP TABLE IF EXISTS pool_credentials;
DROP TABLE IF EXISTS budgets;
DROP TABLE IF EXISTS pools;
DROP TABLE IF EXISTS accounts;
