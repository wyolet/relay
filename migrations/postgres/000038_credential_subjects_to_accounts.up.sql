-- A policy binding naming a host key binds the key's account: a provider counts limits per account, so a limit set on one key is the account's. Existing credential subjects move to their key's account. An account that would end up held by two bindings stops the upgrade, listed, so the operator chooses which binding keeps it.
CREATE TEMP TABLE subject_accounts AS
SELECT s.binding_id, s.position, s.kind,
       CASE WHEN s.kind = 'credential' THEN sec.spec->>'accountId' ELSE s.subject_id END AS account_id
FROM policy_binding_subjects s
LEFT JOIN secrets sec ON s.kind = 'credential' AND sec.id = s.subject_id
WHERE s.kind IN ('account', 'credential');

DO $$
DECLARE
    dups TEXT;
BEGIN
    SELECT string_agg(format('account %s in %s', account_id, bindings), '; ' ORDER BY account_id) INTO dups
    FROM (
        SELECT a.account_id, string_agg(DISTINCT b.name, ', ') AS bindings
        FROM subject_accounts a JOIN policy_bindings b ON b.id = a.binding_id
        WHERE COALESCE(a.account_id, '') <> ''
        GROUP BY a.account_id
        HAVING count(DISTINCT a.binding_id) > 1
    ) d;
    IF dups IS NOT NULL THEN
        RAISE EXCEPTION 'migration 000038: a policy binding on a host key now binds its account, and an account is bound by one policy binding; remove all but one, then upgrade again: %', dups;
    END IF;
END $$;

-- A binding reaching one account twice (two of its keys, or a key and the account) keeps the first.
DELETE FROM policy_binding_subjects s
USING (
    SELECT binding_id, position, row_number() OVER (PARTITION BY binding_id, account_id ORDER BY position) AS n
    FROM subject_accounts WHERE COALESCE(account_id, '') <> ''
) r
WHERE s.binding_id = r.binding_id AND s.position = r.position AND r.n > 1;

UPDATE policy_binding_subjects s
SET kind = 'account', subject_id = a.account_id, subject_account_id = a.account_id, subject_credential_id = NULL
FROM subject_accounts a
WHERE s.binding_id = a.binding_id AND s.position = a.position AND a.kind = 'credential' AND COALESCE(a.account_id, '') <> '';

DROP TABLE subject_accounts;
