-- Preserve failed intents and their terminal cancellation without inventing a trash result.
CREATE TABLE ledger_trash_entries_v2 (
 trash_id TEXT PRIMARY KEY,
 project_id TEXT NOT NULL,
 asset_id TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('pending','trashed','restored','purged','cancelled')),
 revision INTEGER NOT NULL CHECK(revision>0),
 due_at INTEGER NOT NULL,
 hold INTEGER NOT NULL CHECK(hold IN (0,1)),
 record TEXT NOT NULL CHECK(json_valid(record))
) STRICT;
INSERT INTO ledger_trash_entries_v2 SELECT * FROM ledger_trash_entries;
DROP TABLE ledger_trash_entries;
ALTER TABLE ledger_trash_entries_v2 RENAME TO ledger_trash_entries;
CREATE INDEX ledger_trash_due ON ledger_trash_entries(state,hold,due_at);
CREATE TABLE ledger_lifecycle_ops_v2 (
 operation_id TEXT PRIMARY KEY,
 trash_id TEXT NOT NULL,
 action TEXT NOT NULL CHECK(action IN ('trash','restore','purge')),
 command TEXT NOT NULL CHECK(json_valid(command)),
 plan TEXT NOT NULL CHECK(json_valid(plan)),
 state TEXT NOT NULL CHECK(state IN ('accepted','done','cancelled'))
) STRICT;
INSERT INTO ledger_lifecycle_ops_v2 SELECT * FROM ledger_lifecycle_ops;
DROP TABLE ledger_lifecycle_ops;
ALTER TABLE ledger_lifecycle_ops_v2 RENAME TO ledger_lifecycle_ops;
