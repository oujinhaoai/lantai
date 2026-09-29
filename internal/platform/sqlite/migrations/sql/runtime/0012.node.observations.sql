CREATE TABLE node_observations (
 principal_id TEXT NOT NULL,
 project_id TEXT NOT NULL,
 session_id TEXT NOT NULL,
 observed_at INTEGER NOT NULL,
 revision INTEGER NOT NULL CHECK(revision > 0),
 record TEXT NOT NULL CHECK(json_valid(record)),
 PRIMARY KEY(principal_id,project_id)
) STRICT;
