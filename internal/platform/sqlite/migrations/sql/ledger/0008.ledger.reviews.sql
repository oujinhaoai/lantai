CREATE TABLE ledger_review_targets (
 target_id TEXT PRIMARY KEY,
 project_id TEXT NOT NULL,
 asset_id TEXT NOT NULL,
 version_id TEXT NOT NULL,
 candidate_group TEXT NOT NULL,
 record TEXT NOT NULL CHECK(json_valid(record))
);
CREATE INDEX ledger_review_targets_asset ON ledger_review_targets(asset_id);
CREATE TABLE ledger_reviews (
 review_id TEXT PRIMARY KEY,
 target_id TEXT NOT NULL,
 version_id TEXT NOT NULL,
 operation_id TEXT NOT NULL UNIQUE,
 record TEXT NOT NULL CHECK(json_valid(record))
);
CREATE TABLE ledger_publication_requests (
 request_id TEXT PRIMARY KEY,
 review_id TEXT NOT NULL UNIQUE,
 asset_id TEXT NOT NULL,
 version_id TEXT NOT NULL,
 expected_revision INTEGER NOT NULL,
 status TEXT NOT NULL CHECK(status IN ('pending','succeeded','failed','cancelled')),
 failure_code TEXT NOT NULL DEFAULT '',
 record TEXT NOT NULL CHECK(json_valid(record))
);
