CREATE TABLE ledger_projects (
 project_id TEXT PRIMARY KEY, project_key TEXT NOT NULL UNIQUE,
 record TEXT NOT NULL CHECK(json_valid(record))
) STRICT;
CREATE TABLE ledger_assets (
 asset_id TEXT PRIMARY KEY, project_id TEXT NOT NULL, slug TEXT NOT NULL,
 generation INTEGER NOT NULL CHECK(generation > 0), next_number INTEGER NOT NULL CHECK(next_number > 0),
 latest_version_id TEXT NOT NULL DEFAULT '', pending_operation_id TEXT NOT NULL DEFAULT '',
 record TEXT CHECK(record IS NULL OR json_valid(record))
) STRICT;
CREATE TABLE ledger_namespace_claims (
 project_id TEXT NOT NULL, slug_key TEXT NOT NULL,
 record TEXT NOT NULL CHECK(json_valid(record)), previous_record TEXT CHECK(previous_record IS NULL OR json_valid(previous_record)),
 PRIMARY KEY(project_id, slug_key)
) STRICT;
CREATE TABLE ledger_prepared (
 operation_id TEXT PRIMARY KEY, command TEXT NOT NULL CHECK(json_valid(command)),
 prepared TEXT NOT NULL CHECK(json_valid(prepared)), base_version_id TEXT NOT NULL,
 proof TEXT CHECK(proof IS NULL OR json_valid(proof))
) STRICT;
CREATE TABLE ledger_versions (
 version_id TEXT PRIMARY KEY, asset_id TEXT NOT NULL, project_id TEXT NOT NULL,
 version_number INTEGER NOT NULL CHECK(version_number > 0), operation_id TEXT NOT NULL UNIQUE,
 record TEXT NOT NULL CHECK(json_valid(record)), UNIQUE(asset_id, version_number)
) STRICT;
CREATE TABLE ledger_version_states (
 version_id TEXT PRIMARY KEY, revision INTEGER NOT NULL CHECK(revision > 0),
 state TEXT NOT NULL CHECK(state = 'draft')
) STRICT;
CREATE TABLE ledger_metadata_targets (
 kind TEXT NOT NULL CHECK(kind IN ('asset', 'project')), target_id TEXT NOT NULL,
 project_id TEXT NOT NULL, revision INTEGER NOT NULL DEFAULT 0 CHECK(revision >= 0),
 pending_operation_id TEXT NOT NULL DEFAULT '', record TEXT CHECK(record IS NULL OR json_valid(record)),
 PRIMARY KEY(kind, target_id)
) STRICT;
CREATE TABLE ledger_metadata_prepared (
 operation_id TEXT PRIMARY KEY, command TEXT NOT NULL CHECK(json_valid(command)),
 request TEXT NOT NULL CHECK(json_valid(request)), prepared TEXT NOT NULL CHECK(json_valid(prepared)),
 proof_digest TEXT
) STRICT;
CREATE TABLE ledger_metadata_revisions (
 operation_id TEXT PRIMARY KEY, kind TEXT NOT NULL, target_id TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK(revision > 0), record TEXT NOT NULL CHECK(json_valid(record)),
 UNIQUE(kind, target_id, revision)
) STRICT;
