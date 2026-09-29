CREATE TABLE ledger_trash_entries (
 trash_id TEXT PRIMARY KEY,
 project_id TEXT NOT NULL,
 asset_id TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('pending','trashed','restored','purged')),
 revision INTEGER NOT NULL CHECK(revision>0),
 due_at INTEGER NOT NULL,
 hold INTEGER NOT NULL CHECK(hold IN (0,1)),
 record TEXT NOT NULL CHECK(json_valid(record))
) STRICT;
CREATE INDEX ledger_trash_due ON ledger_trash_entries(state,hold,due_at);
CREATE TABLE ledger_lifecycle_ops (
 operation_id TEXT PRIMARY KEY,
 trash_id TEXT NOT NULL,
 action TEXT NOT NULL CHECK(action IN ('trash','restore','purge')),
 command TEXT NOT NULL CHECK(json_valid(command)),
 plan TEXT NOT NULL CHECK(json_valid(plan)),
 state TEXT NOT NULL CHECK(state IN ('accepted','done'))
) STRICT;
CREATE TABLE ledger_trash_quota (
 operation_id TEXT PRIMARY KEY,
 principal_id TEXT NOT NULL,
 created_at INTEGER NOT NULL,
 units INTEGER NOT NULL CHECK(units>0),
 grace INTEGER NOT NULL CHECK(grace IN (0,1)),
 state TEXT NOT NULL CHECK(state IN ('reserved','committed','released'))
) STRICT;
CREATE INDEX ledger_trash_quota_window ON ledger_trash_quota(principal_id,created_at);
-- Base state is a final-acceptance fence, separate from immutable content.
ALTER TABLE ledger_prepared ADD COLUMN base_control_revision INTEGER NOT NULL DEFAULT 0 CHECK(base_control_revision>=0);
ALTER TABLE ledger_metadata_prepared ADD COLUMN asset_control_revision INTEGER NOT NULL DEFAULT 0 CHECK(asset_control_revision>=0);
CREATE TABLE ledger_alias_allocations (
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,
 project_id TEXT NOT NULL,
 slug_key TEXT NOT NULL,
 generation INTEGER NOT NULL CHECK(generation>0),
 record TEXT NOT NULL CHECK(json_valid(record)),
 UNIQUE(project_id,slug_key,generation)
) STRICT;
CREATE TABLE ledger_gc_candidates (
 sha256 TEXT PRIMARY KEY,
 operation_id TEXT NOT NULL UNIQUE,
 since INTEGER NOT NULL
) STRICT;
CREATE TABLE ledger_trash_reminders (
 trash_id TEXT PRIMARY KEY,
 operation_id TEXT NOT NULL,
 created_at INTEGER NOT NULL
) STRICT;
CREATE TABLE ledger_lifecycle_failures (
 operation_id TEXT PRIMARY KEY,
 code TEXT NOT NULL,
 attempts INTEGER NOT NULL CHECK(attempts>0),
 last_at INTEGER NOT NULL
) STRICT;
