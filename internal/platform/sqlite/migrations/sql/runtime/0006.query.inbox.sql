CREATE TABLE query_inbox_items (
 principal_id TEXT NOT NULL,
 project_id TEXT NOT NULL,
 object_kind TEXT NOT NULL,
 object_id TEXT NOT NULL,
 event_id TEXT NOT NULL,
 global_seq INTEGER NOT NULL,
 PRIMARY KEY(principal_id, project_id, object_kind, object_id)
) STRICT;
CREATE TABLE query_inbox_read (
 principal_id TEXT NOT NULL,
 project_id TEXT NOT NULL,
 through_seq INTEGER NOT NULL CHECK(through_seq >= 0),
 PRIMARY KEY(principal_id, project_id)
) STRICT;
