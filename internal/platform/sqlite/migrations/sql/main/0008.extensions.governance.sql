-- Independently imported packages. Bytes stay in the committed plugin asset
-- version; review facts stay in ledger. These rows are T09 registration only.
CREATE TABLE extensions_packages (
 extension_id TEXT NOT NULL,
 extension_version TEXT NOT NULL,
 package_digest TEXT NOT NULL,
 manifest_digest TEXT NOT NULL,
 record TEXT NOT NULL CHECK(json_valid(record)),
 PRIMARY KEY(extension_id,extension_version)
) STRICT;
-- Desired enablement per plugin/target/scope. generation changes on every
-- enable, upgrade, config change or disable; history keeps retirement mode.
CREATE TABLE extensions_enablements (
 enablement_id TEXT PRIMARY KEY,
 extension_id TEXT NOT NULL,
 target TEXT NOT NULL CHECK(target IN ('server','cli')),
 scope_kind TEXT NOT NULL CHECK(scope_kind IN ('instance','project','user')),
 scope_id TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK(revision>0),
 generation INTEGER NOT NULL CHECK(generation>0),
 state TEXT NOT NULL CHECK(state IN ('enabled','disabled')),
 record TEXT NOT NULL CHECK(json_valid(record)),
 UNIQUE(extension_id,target,scope_kind,scope_id)
) STRICT;
CREATE TABLE extensions_enablement_history (
 enablement_id TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK(revision>0),
 record TEXT NOT NULL CHECK(json_valid(record)),
 PRIMARY KEY(enablement_id,revision)
) STRICT;
