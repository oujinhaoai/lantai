CREATE TABLE agent_execution_runs (
    run_id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    revision INTEGER NOT NULL CHECK(revision > 0),
    record TEXT NOT NULL CHECK(json_valid(record))
) STRICT;
CREATE INDEX agent_execution_runs_task ON agent_execution_runs(task_id, run_id);
CREATE TABLE agent_execution_history (
    run_id TEXT NOT NULL,
    revision INTEGER NOT NULL,
    record TEXT NOT NULL CHECK(json_valid(record)),
    PRIMARY KEY(run_id, revision)
) STRICT;
CREATE TABLE agent_execution_admissions (
    execution_key TEXT PRIMARY KEY,
    execution_id TEXT NOT NULL UNIQUE,
    run_id TEXT NOT NULL,
    attempt_id TEXT NOT NULL UNIQUE,
    session_id TEXT NOT NULL,
    request TEXT NOT NULL CHECK(json_valid(request)),
    admission TEXT NOT NULL CHECK(json_valid(admission)),
    run_revision INTEGER NOT NULL
) STRICT;
