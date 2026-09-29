CREATE TABLE storage_file_actions (
 operation_id TEXT PRIMARY KEY,
 intent_digest TEXT NOT NULL,
 intent_json TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('ready','done')),
 completed_at INTEGER
) STRICT;
CREATE TABLE storage_gc_deletions (
 operation_id TEXT PRIMARY KEY,
 sha256 TEXT NOT NULL,
 candidate_at INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('deleting','done','cancelled')),
 completed_at INTEGER
) STRICT;
