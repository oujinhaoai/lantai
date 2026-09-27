CREATE TABLE query_state (
 singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
 generation INTEGER NOT NULL DEFAULT 0 CHECK (generation >= 0),
 high_water INTEGER NOT NULL DEFAULT 0 CHECK (high_water >= 0),
 rebuild_start INTEGER NOT NULL DEFAULT 0 CHECK (rebuild_start >= 0)
);
INSERT INTO query_state(singleton) VALUES (1);
CREATE TABLE query_assets (
 asset_id TEXT PRIMARY KEY,
 project_id TEXT NOT NULL,
 latest_version_id TEXT NOT NULL,
 document_json BLOB NOT NULL,
 search_text TEXT NOT NULL
);
CREATE INDEX query_assets_project ON query_assets(project_id, asset_id);
CREATE TABLE query_relations (
 source_asset_id TEXT NOT NULL,
 source_version_id TEXT NOT NULL,
 target_instance_id TEXT NOT NULL,
 target_asset_id TEXT NOT NULL,
 target_version_id TEXT NOT NULL,
 relation TEXT NOT NULL,
 PRIMARY KEY(source_version_id, target_asset_id, target_version_id, relation)
);
CREATE INDEX query_relations_target ON query_relations(target_asset_id, target_version_id);
CREATE TABLE query_revisions (
 aggregate_type TEXT NOT NULL,
 aggregate_id TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK (revision > 0),
 PRIMARY KEY(aggregate_type, aggregate_id)
);
CREATE TABLE query_processed_events (
 event_id TEXT PRIMARY KEY,
 global_seq INTEGER NOT NULL UNIQUE CHECK (global_seq > 0)
);
