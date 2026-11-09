-- Reverse 000041. Accounts still marked grouped_by_relay become their owner's default account at their host again (the first by id when an owner has two there); one whose org was confirmed since stays a plain account. Keys keep the account they name.

ALTER TABLE accounts ADD COLUMN IF NOT EXISTS default_owner TEXT;
UPDATE accounts a SET default_owner = d.scope
FROM (
    SELECT DISTINCT ON (host_id, scope) id, scope
    FROM (
        SELECT id, host_id,
               CASE WHEN COALESCE(metadata->'owner'->>'kind', '') IN ('', 'system') THEN 'system'
                    ELSE (metadata->'owner'->>'kind') || ':' || COALESCE(metadata->'owner'->>'id', '') END AS scope
        FROM accounts
        WHERE grouped_by_relay
    ) g
    ORDER BY host_id, scope, id
) d
WHERE a.id = d.id;
CREATE UNIQUE INDEX IF NOT EXISTS accounts_one_default ON accounts (host_id, default_owner);

CREATE OR REPLACE FUNCTION accounts_drop_keyless_defaults() RETURNS trigger AS $$
BEGIN
    DELETE FROM accounts a
    WHERE a.default_owner = TG_ARGV[0] || ':' || OLD.id
      AND NOT EXISTS (SELECT 1 FROM secrets s WHERE s.spec->>'accountId' = a.id);
    RETURN NULL;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER teams_drop_keyless_default_accounts AFTER DELETE ON teams
    FOR EACH ROW EXECUTE FUNCTION accounts_drop_keyless_defaults('team');
CREATE TRIGGER projects_drop_keyless_default_accounts AFTER DELETE ON projects
    FOR EACH ROW EXECUTE FUNCTION accounts_drop_keyless_defaults('project');

ALTER TABLE accounts DROP COLUMN IF EXISTS grouped_by_relay;
