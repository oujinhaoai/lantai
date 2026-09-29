ALTER TABLE ledger_version_states RENAME TO ledger_version_states_m1;
CREATE TABLE ledger_version_states (
 version_id TEXT PRIMARY KEY,
 revision INTEGER NOT NULL CHECK(revision > 0),
 state TEXT NOT NULL CHECK(state IN ('draft','submitted','approved','changes_requested','rejected','withdrawn','not_selected','abandoned')),
 availability TEXT NOT NULL DEFAULT 'enabled' CHECK(availability IN ('enabled','disabled')),
 disabled_reason TEXT NOT NULL DEFAULT '',
 lifecycle TEXT NOT NULL DEFAULT 'active' CHECK(lifecycle IN ('active','archived','trashed','purged')),
 effective_review_id TEXT NOT NULL DEFAULT '',
 review_target_id TEXT NOT NULL DEFAULT '',
 withdrawal_reason TEXT NOT NULL DEFAULT '',
 pending_operation_id TEXT NOT NULL DEFAULT ''
) STRICT;
INSERT INTO ledger_version_states(version_id,revision,state) SELECT version_id,revision,state FROM ledger_version_states_m1;
DROP TABLE ledger_version_states_m1;
CREATE TABLE ledger_asset_controls (
 asset_id TEXT PRIMARY KEY,
 revision INTEGER NOT NULL CHECK(revision > 0),
 lifecycle TEXT NOT NULL CHECK(lifecycle IN ('active','archived','trashed','purged')),
 publication_state TEXT NOT NULL CHECK(publication_state IN ('unpublished','published','suspended')),
 published_version_id TEXT NOT NULL DEFAULT '',
 publication_revision INTEGER NOT NULL DEFAULT 0,
 pending_operation_id TEXT NOT NULL DEFAULT ''
) STRICT;
CREATE TABLE ledger_locks (
 lock_id TEXT PRIMARY KEY,
 project_id TEXT NOT NULL,
 scope TEXT NOT NULL CHECK(scope IN ('asset','path')),
 target TEXT NOT NULL,
 revision INTEGER NOT NULL,
 active INTEGER NOT NULL CHECK(active IN (0,1)),
 record TEXT NOT NULL CHECK(json_valid(record))
) STRICT;
CREATE INDEX ledger_locks_scope ON ledger_locks(project_id,active,scope,target);
