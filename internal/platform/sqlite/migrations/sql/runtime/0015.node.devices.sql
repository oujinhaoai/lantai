-- T06 node owns authenticated device/workspace observations and pairing only.
-- Job state, attempt, lease and fences stay in jobs_jobs/jobs_attempts.
CREATE TABLE node_devices (
 device_id TEXT PRIMARY KEY,
 project_id TEXT NOT NULL,
 owner_id TEXT NOT NULL,
 session_id TEXT NOT NULL UNIQUE,
 state TEXT NOT NULL,
 record TEXT NOT NULL CHECK(json_valid(record))
) STRICT;
CREATE INDEX node_devices_owner ON node_devices(project_id,owner_id,device_id);
