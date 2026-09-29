ALTER TABLE storage_read_grants ADD COLUMN version_fence INTEGER NOT NULL DEFAULT 0 CHECK(version_fence>=0);
