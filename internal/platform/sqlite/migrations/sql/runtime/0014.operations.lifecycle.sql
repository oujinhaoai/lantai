-- T08 scheduler registry: each server lifecycle job fixes its operation,
-- idempotency key, request digest and recovery epoch before the owning module
-- runs it. Business decisions and receipts stay in ledger/storage.
CREATE TABLE operations_lifecycle_jobs (
 job_key TEXT PRIMARY KEY,
 kind TEXT NOT NULL CHECK(kind IN ('ledger.purge_due','ledger.purge_reminder','storage.gc')),
 operation_id TEXT NOT NULL UNIQUE,
 state TEXT NOT NULL CHECK(state IN ('pending','done','superseded','failed')),
 attempts INTEGER NOT NULL CHECK(attempts>=0),
 next_at INTEGER NOT NULL,
 record TEXT NOT NULL CHECK(json_valid(record))
) STRICT;
CREATE INDEX operations_lifecycle_jobs_due ON operations_lifecycle_jobs(state,next_at);
