-- The older binary reads a key's policy from spec.policyId only. A service
-- account key that resolved one through its account or a policy binding gets
-- it written there, in the data plane's order: the account's own policy, then
-- the first matching binding by (priority, name).
UPDATE relay_keys k
   SET spec = jsonb_set(k.spec, '{policyId}', to_jsonb(r.policy_id), true)
  FROM (
    SELECT k2.id AS key_id,
           coalesce(
             (SELECT pol.id FROM policies pol WHERE pol.id = sa.spec->>'policyId'),
             (SELECT pb.policy_id
                FROM policy_bindings pb
               WHERE pb.project_id = sa.project_id
                 AND EXISTS (
                   SELECT 1 FROM policy_binding_subjects s
                    WHERE s.binding_id = pb.id
                      AND ((s.kind = 'serviceaccount' AND s.subject_id = sa.id)
                        OR (s.kind = 'group' AND s.subject_name IN (
                              'system:serviceaccounts',
                              'system:serviceaccounts:' || p.name,
                              'system:authenticated'))))
               ORDER BY pb.priority, pb.name
               LIMIT 1)) AS policy_id
      FROM relay_keys k2
      JOIN service_accounts sa ON sa.id = k2.principal_sa_id
      JOIN projects p ON p.id = sa.project_id
     WHERE coalesce(k2.spec->>'policyId', '') = ''
  ) r
 WHERE k.id = r.key_id
   AND r.policy_id IS NOT NULL;

DROP TRIGGER IF EXISTS policy_binding_subjects_notify ON policy_binding_subjects;
DROP TRIGGER IF EXISTS role_binding_subjects_notify ON role_binding_subjects;
DROP TRIGGER IF EXISTS policy_bindings_notify ON policy_bindings;
DROP TRIGGER IF EXISTS role_bindings_notify ON role_bindings;
DROP TRIGGER IF EXISTS roles_notify ON roles;
DROP TABLE IF EXISTS policy_binding_subjects;
DROP TABLE IF EXISTS role_binding_subjects;
DROP TABLE IF EXISTS policy_bindings;
DROP TABLE IF EXISTS role_bindings;
DROP TABLE IF EXISTS roles;
