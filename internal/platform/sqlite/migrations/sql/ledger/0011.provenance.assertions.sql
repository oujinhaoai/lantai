-- Evidence bytes remain immutable storage records. These rows accept only their
-- hashes, revisions and the fixed command used to activate them.
CREATE TABLE provenance_assertion_targets (
 version_id TEXT PRIMARY KEY,
 revision INTEGER NOT NULL CHECK(revision>=2),
 assertion_id TEXT NOT NULL
) STRICT;
CREATE TABLE provenance_assertion_prepared (
 operation_id TEXT PRIMARY KEY,
 command TEXT NOT NULL CHECK(json_valid(command)),
 request TEXT NOT NULL CHECK(json_valid(request)),
 record TEXT NOT NULL CHECK(json_valid(record)),
 expected_revision INTEGER NOT NULL CHECK(expected_revision>=1)
) STRICT;
CREATE TABLE provenance_assertions (
 assertion_id TEXT PRIMARY KEY,
 version_id TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK(revision>=2),
 digest TEXT NOT NULL,
 operation_id TEXT NOT NULL UNIQUE,
 evidence_revision INTEGER NOT NULL CHECK(evidence_revision>=0),
 UNIQUE(version_id,revision)
) STRICT;
CREATE TABLE provenance_rights_epoch (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 epoch INTEGER NOT NULL CHECK(epoch>=1)
) STRICT;
INSERT INTO provenance_rights_epoch SELECT 1, COALESCE(max(global_seq),0)+1 FROM provenance_records;
