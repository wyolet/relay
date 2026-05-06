-- M9: attachments table removed; rate-limit attachments are now expressed inline
-- on Pool/Secret/Model specs and derived at runtime from the snapshot.
DROP TABLE IF EXISTS attachments;
