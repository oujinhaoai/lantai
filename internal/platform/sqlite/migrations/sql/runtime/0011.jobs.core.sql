CREATE TABLE jobs_jobs (
 job_id TEXT PRIMARY KEY,
 project_id TEXT NOT NULL,
 state TEXT NOT NULL,
 record TEXT NOT NULL CHECK(json_valid(record))
) STRICT;
CREATE INDEX jobs_queue ON jobs_jobs(project_id,state,job_id);
CREATE TABLE jobs_attempts (
 attempt_id TEXT PRIMARY KEY,
 job_id TEXT NOT NULL,
 record TEXT NOT NULL CHECK(json_valid(record))
) STRICT;
