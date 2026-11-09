-- Reverse 000037. User-owned bindings have no owner below it, so they are deleted.
DELETE FROM policy_bindings WHERE user_id IS NOT NULL;
DROP INDEX IF EXISTS policy_bindings_user_idx;
ALTER TABLE policy_bindings DROP CONSTRAINT IF EXISTS policy_bindings_one_owner;
ALTER TABLE policy_bindings DROP COLUMN IF EXISTS user_id;
ALTER TABLE policy_bindings ADD CONSTRAINT policy_bindings_one_owner CHECK (project_id IS NULL OR team_id IS NULL);
