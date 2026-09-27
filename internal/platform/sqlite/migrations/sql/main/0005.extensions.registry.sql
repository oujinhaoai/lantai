CREATE TABLE extensions_registrations (
 extension_id TEXT NOT NULL,
 extension_version TEXT NOT NULL,
 package_digest TEXT NOT NULL,
 manifest_digest TEXT NOT NULL,
 manifest_json BLOB NOT NULL,
 source TEXT NOT NULL CHECK(source='builtin_release'),
 PRIMARY KEY(extension_id,extension_version)
);
CREATE TABLE extensions_release_bindings (
 extension_id TEXT NOT NULL,
 extension_version TEXT NOT NULL,
 core_release_digest TEXT NOT NULL,
 package_digest TEXT NOT NULL,
 PRIMARY KEY(extension_id,extension_version,core_release_digest)
);
CREATE TABLE extensions_contributions (
 contribution_id TEXT NOT NULL,
 extension_id TEXT NOT NULL,
 extension_version TEXT NOT NULL,
 point_id TEXT NOT NULL,
 target TEXT NOT NULL,
 PRIMARY KEY(contribution_id,extension_version)
);
