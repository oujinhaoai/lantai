CREATE TABLE workflow_flows (
 flow_id TEXT PRIMARY KEY,
 project_id TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK(revision >= 1),
 state TEXT NOT NULL,
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL,
 record TEXT NOT NULL CHECK(json_valid(record)),
 meta TEXT NOT NULL CHECK(json_valid(meta))
) STRICT;
CREATE INDEX workflow_flows_project ON workflow_flows(project_id, flow_id);
CREATE INDEX workflow_flows_open ON workflow_flows(project_id, flow_id) WHERE state NOT IN ('completed','cancelled');
CREATE TABLE workflow_step_runs (
 step_run_id TEXT PRIMARY KEY,
 flow_id TEXT NOT NULL,
 step_key TEXT NOT NULL,
 round INTEGER NOT NULL CHECK(round >= 1),
 ordinal INTEGER NOT NULL,
 revision INTEGER NOT NULL CHECK(revision >= 1),
 state TEXT NOT NULL,
 record TEXT NOT NULL CHECK(json_valid(record)),
 meta TEXT NOT NULL CHECK(json_valid(meta)),
 UNIQUE(flow_id, step_key, round)
) STRICT;
CREATE INDEX workflow_step_runs_flow ON workflow_step_runs(flow_id, round, ordinal);
CREATE TABLE workflow_bindings (
 kind TEXT NOT NULL CHECK(kind IN ('task','job','asset','version')),
 ref_id TEXT NOT NULL,
 flow_id TEXT NOT NULL,
 step_run_id TEXT NOT NULL,
 PRIMARY KEY(kind, ref_id, step_run_id)
) STRICT;
CREATE INDEX workflow_bindings_flow ON workflow_bindings(flow_id);
CREATE TABLE workflow_commands (
 operation_id TEXT PRIMARY KEY,
 flow_id TEXT NOT NULL,
 step_run_id TEXT NOT NULL,
 command_type TEXT NOT NULL,
 payload TEXT NOT NULL CHECK(json_valid(payload)),
 status TEXT NOT NULL CHECK(status IN ('pending','succeeded','blocked','cancelled')),
 actor_id TEXT NOT NULL DEFAULT '',
 attempts INTEGER NOT NULL DEFAULT 0,
 retry_at INTEGER NOT NULL DEFAULT 0,
 failure_code TEXT NOT NULL DEFAULT '',
 receipt TEXT,
 created_at INTEGER NOT NULL
) STRICT;
CREATE INDEX workflow_commands_pending ON workflow_commands(retry_at) WHERE status = 'pending';
CREATE INDEX workflow_commands_flow ON workflow_commands(flow_id);
