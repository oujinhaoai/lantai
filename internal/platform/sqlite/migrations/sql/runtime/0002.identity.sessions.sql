-- identity（T01）在 runtime.db 的会话运行记录。会话令牌只存 SHA-256；
-- 会话是否仍有效在每次验证时对照 main.db 的主体状态、auth_epoch 与凭据，
-- 以及实例当前的 recovery_epoch，不依赖缓存。
CREATE TABLE identity_sessions (
	session_id          TEXT    PRIMARY KEY,
	token_hash          TEXT    NOT NULL UNIQUE,
	principal_id        TEXT    NOT NULL,
	principal_kind      TEXT    NOT NULL CHECK (principal_kind IN ('human', 'agent', 'node', 'worker', 'runner', 'service')),
	session_kind        TEXT    NOT NULL CHECK (session_kind IN ('normal', 'recovery', 'delegated')),
	channel             TEXT    NOT NULL CHECK (channel IN ('cli', 'browser', 'api')),
	credential_id       TEXT,
	parent_session_id   TEXT,
	delegated_by        TEXT,
	delegate_auth_epoch INTEGER,
	purpose             TEXT    NOT NULL DEFAULT '',
	model               TEXT    NOT NULL DEFAULT '',
	scopes              TEXT    NOT NULL,
	projects            TEXT    NOT NULL DEFAULT '[]',
	actions             TEXT    NOT NULL DEFAULT '[]',
	operation_id        TEXT,
	auth_epoch          INTEGER NOT NULL CHECK (auth_epoch >= 1),
	recovery_epoch      INTEGER NOT NULL CHECK (recovery_epoch >= 1),
	created_at          INTEGER NOT NULL,
	expires_at          INTEGER NOT NULL,
	ended_at            INTEGER,
	end_reason          TEXT,
	CHECK ((session_kind = 'delegated') = (delegated_by IS NOT NULL)),
	CHECK ((session_kind = 'recovery') = (operation_id IS NOT NULL))
) STRICT;

CREATE INDEX identity_sessions_open_by_principal ON identity_sessions (principal_id) WHERE ended_at IS NULL;
CREATE INDEX identity_sessions_by_parent ON identity_sessions (parent_session_id) WHERE parent_session_id IS NOT NULL;
