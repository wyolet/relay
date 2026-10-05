-- Rows written before the actor's session id was derived from the session
-- token hold the token itself. Replace each with the id the session layer now
-- derives (hex of the first 16 bytes of its SHA-256), so the stored value no
-- longer authenticates while rows of one session still correlate.
UPDATE audit_events
SET session_id = left(encode(sha256(convert_to(session_id, 'UTF8')), 'hex'), 32)
WHERE session_id IS NOT NULL
  AND session_id !~ '^[0-9a-f]{32}$';
