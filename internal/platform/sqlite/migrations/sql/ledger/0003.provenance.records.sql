CREATE TABLE provenance_targets (
 version_id TEXT PRIMARY KEY,
 revision INTEGER NOT NULL CHECK(revision > 0)
) STRICT;
CREATE TABLE provenance_prepared (
 operation_id TEXT PRIMARY KEY,
 version_id TEXT NOT NULL,
 expected_revision INTEGER NOT NULL CHECK(expected_revision >= 0),
 record_json TEXT NOT NULL
) STRICT;
CREATE TABLE provenance_records (
 global_seq INTEGER PRIMARY KEY AUTOINCREMENT,
 record_id TEXT NOT NULL UNIQUE,
 version_id TEXT NOT NULL,
 digest TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK(revision > 0),
 operation_id TEXT NOT NULL UNIQUE,
 UNIQUE(version_id,revision)
) STRICT;
