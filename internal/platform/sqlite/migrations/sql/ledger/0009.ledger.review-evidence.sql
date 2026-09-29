CREATE TABLE ledger_check_prepared (
 operation_id TEXT PRIMARY KEY,
 record_json TEXT NOT NULL CHECK(json_valid(record_json))
);
CREATE TABLE ledger_check_records (
 evidence_id TEXT PRIMARY KEY,
 project_id TEXT NOT NULL,
 asset_id TEXT NOT NULL,
 version_id TEXT NOT NULL,
 digest TEXT NOT NULL,
 operation_id TEXT NOT NULL UNIQUE
);
CREATE INDEX ledger_check_records_version ON ledger_check_records(version_id);
