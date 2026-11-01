-- Reverse 000035. Policy-binding subjects of the added kinds are deleted: the restored check refuses them. A HostKey's spec.accountId is left in place; older releases ignore it.

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
