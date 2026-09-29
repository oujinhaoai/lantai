CREATE TABLE tasks_tasks (
 task_id TEXT PRIMARY KEY,
 project_id TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK(revision >= 1),
 state TEXT NOT NULL,
 type TEXT NOT NULL,
 flow_id TEXT NOT NULL DEFAULT '',
 step_run_id TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL,
 record TEXT NOT NULL CHECK(json_valid(record)),
 meta TEXT NOT NULL CHECK(json_valid(meta))
) STRICT;
CREATE INDEX tasks_tasks_project ON tasks_tasks(project_id, task_id);
CREATE INDEX tasks_tasks_open ON tasks_tasks(project_id, task_id) WHERE state NOT IN ('done','cancelled');
CREATE UNIQUE INDEX tasks_tasks_step ON tasks_tasks(step_run_id) WHERE step_run_id <> '';
CREATE TABLE tasks_dependencies (
 task_id TEXT NOT NULL,
 depends_on TEXT NOT NULL,
 PRIMARY KEY(task_id, depends_on)
) STRICT;
CREATE INDEX tasks_dependencies_reverse ON tasks_dependencies(depends_on);
CREATE TABLE tasks_seats (
 seat_id TEXT PRIMARY KEY,
 task_id TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK(revision >= 1),
 state TEXT NOT NULL,
 record TEXT NOT NULL CHECK(json_valid(record))
) STRICT;
CREATE INDEX tasks_seats_task ON tasks_seats(task_id);
CREATE TABLE tasks_attempts (
 attempt_id TEXT PRIMARY KEY,
 task_id TEXT NOT NULL,
 seat_id TEXT NOT NULL,
 round INTEGER NOT NULL CHECK(round >= 1),
 revision INTEGER NOT NULL CHECK(revision >= 1),
 state TEXT NOT NULL,
 principal_id TEXT NOT NULL,
 session_id TEXT NOT NULL,
 lease_fence INTEGER NOT NULL CHECK(lease_fence >= 1),
 expires_at INTEGER NOT NULL,
 record TEXT NOT NULL CHECK(json_valid(record)),
 UNIQUE(seat_id, lease_fence)
) STRICT;
CREATE INDEX tasks_attempts_task ON tasks_attempts(task_id, lease_fence);
CREATE INDEX tasks_attempts_active ON tasks_attempts(principal_id) WHERE state = 'active';
CREATE INDEX tasks_attempts_expiry ON tasks_attempts(expires_at) WHERE state = 'active';
CREATE TABLE tasks_checkouts (
 task_id TEXT NOT NULL,
 asset_id TEXT NOT NULL,
 project_id TEXT NOT NULL,
 mode TEXT NOT NULL CHECK(mode IN ('exclusive','advisory')),
 attempt_id TEXT NOT NULL,
 base_version_id TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('active','released')),
 acquired_at INTEGER NOT NULL,
 released_at INTEGER,
 PRIMARY KEY(task_id, asset_id)
) STRICT;
CREATE UNIQUE INDEX tasks_checkouts_exclusive ON tasks_checkouts(asset_id) WHERE state = 'active' AND mode = 'exclusive';
CREATE INDEX tasks_checkouts_active ON tasks_checkouts(asset_id) WHERE state = 'active';
CREATE TABLE tasks_blocks (
 block_id TEXT PRIMARY KEY,
 task_id TEXT NOT NULL,
 attempt_id TEXT NOT NULL,
 round INTEGER NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('open','answered','withdrawn')),
 record TEXT NOT NULL CHECK(json_valid(record))
) STRICT;
CREATE INDEX tasks_blocks_task ON tasks_blocks(task_id);
CREATE TABLE tasks_handoffs (
 handoff_id TEXT PRIMARY KEY,
 task_id TEXT NOT NULL,
 attempt_id TEXT NOT NULL UNIQUE,
 record TEXT NOT NULL CHECK(json_valid(record))
) STRICT;
CREATE INDEX tasks_handoffs_task ON tasks_handoffs(task_id);
CREATE TABLE tasks_refs (
 task_id TEXT NOT NULL,
 asset_id TEXT NOT NULL,
 version_id TEXT NOT NULL,
 role TEXT NOT NULL CHECK(role IN ('input','output','context','draft','checkout')),
 PRIMARY KEY(task_id, asset_id, version_id, role)
) STRICT;
CREATE INDEX tasks_refs_asset ON tasks_refs(asset_id);
