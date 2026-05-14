-- Reverse 000015: pull spec.hostId back into metadata.owner.{kind=host,id=...}.

UPDATE secrets
SET metadata = jsonb_set(
        metadata,
        '{owner}',
        jsonb_build_object(
            'kind', 'host',
            'id',   COALESCE(spec->>'hostId', '')
        ),
        true
    ),
    spec = spec - 'hostId';
