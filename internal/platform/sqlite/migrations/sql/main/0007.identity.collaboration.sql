CREATE TABLE identity_human_batches (
 operation_id TEXT PRIMARY KEY,
 request_json TEXT NOT NULL
) STRICT;
CREATE TABLE identity_human_grant_items (
 grant_id TEXT NOT NULL,
 child_operation_id TEXT NOT NULL,
 action TEXT NOT NULL,
 target_digest TEXT NOT NULL,
 request_hash TEXT NOT NULL,
 expected_revision INTEGER NOT NULL,
 PRIMARY KEY (grant_id, child_operation_id)
) STRICT;
CREATE TABLE identity_milestones (
 milestone_id TEXT PRIMARY KEY,
 project_id TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK (revision > 0),
 name TEXT NOT NULL,
 due_date TEXT NOT NULL,
 owner_id TEXT NOT NULL,
 task_ids TEXT NOT NULL,
 updated_at INTEGER NOT NULL
) STRICT;
CREATE INDEX identity_milestones_project ON identity_milestones(project_id, milestone_id);
