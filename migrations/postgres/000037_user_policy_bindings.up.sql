-- A policy binding may be owned by a user (user_id), binding that user's own principal; deleting the user deletes the binding. Its subject's owner_scope is 'user:<id>', so the one-binding-per-scope index holds a user to one binding of their own.
ALTER TABLE policy_bindings ADD COLUMN IF NOT EXISTS user_id TEXT REFERENCES users(id) ON DELETE CASCADE;
ALTER TABLE policy_bindings DROP CONSTRAINT IF EXISTS policy_bindings_one_owner;
ALTER TABLE policy_bindings ADD CONSTRAINT policy_bindings_one_owner CHECK (num_nonnulls(project_id, team_id, user_id) <= 1);
CREATE INDEX IF NOT EXISTS policy_bindings_user_idx ON policy_bindings (user_id);
