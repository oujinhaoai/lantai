CREATE TABLE ledger_publications (
 publication_id TEXT PRIMARY KEY,
 asset_id TEXT NOT NULL,
 revision INTEGER NOT NULL,
 operation_id TEXT NOT NULL,
 record TEXT NOT NULL CHECK(json_valid(record)),
 UNIQUE(asset_id,revision)
) STRICT;
