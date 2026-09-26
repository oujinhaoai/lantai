-- identity（T01）在 main.db 的权威表：主体、角色、策略、长期凭据、认证因子、
-- 恢复与设置码、Challenge/HumanGrant 与认证失败计数。私密认证资料只存校验值
-- 或经主密钥加密的密文，不写入事件、素材目录或日志。

-- 实例级身份状态：授权相关修订与一次性初始化。
CREATE TABLE identity_state (
	id                  INTEGER PRIMARY KEY CHECK (id = 1),
	policy_revision     INTEGER NOT NULL CHECK (policy_revision >= 1),
	bootstrap_state     TEXT    NOT NULL CHECK (bootstrap_state IN ('open', 'closed')),
	bootstrap_admin_id  TEXT,
	bootstrap_closed_at INTEGER,
	CHECK ((bootstrap_state = 'open') = (bootstrap_admin_id IS NULL))
) STRICT;

INSERT INTO identity_state (id, policy_revision, bootstrap_state) VALUES (1, 1, 'open');

CREATE TABLE identity_principals (
	principal_id    TEXT    PRIMARY KEY,
	kind            TEXT    NOT NULL CHECK (kind IN ('human', 'agent', 'node', 'worker', 'runner', 'service')),
	name            TEXT    NOT NULL UNIQUE,
	display_name    TEXT    NOT NULL DEFAULT '',
	state           TEXT    NOT NULL CHECK (state IN ('active', 'disabled')),
	auth_epoch      INTEGER NOT NULL CHECK (auth_epoch >= 1),
	profile         TEXT    NOT NULL DEFAULT '{}',
	revision        INTEGER NOT NULL CHECK (revision >= 1),
	created_at      INTEGER NOT NULL,
	created_by      TEXT,
	updated_at      INTEGER NOT NULL,
	disabled_reason TEXT
) STRICT;

CREATE TABLE identity_system_roles (
	principal_id TEXT    NOT NULL,
	role         TEXT    NOT NULL CHECK (role IN ('admin')),
	granted_at   INTEGER NOT NULL,
	granted_by   TEXT,
	PRIMARY KEY (principal_id, role)
) STRICT;

-- 项目的访问控制修订；项目的稳定 ID 与说明归 catalog，这里只记成员与策略的修订。
CREATE TABLE identity_projects (
	project_id TEXT    PRIMARY KEY,
	revision   INTEGER NOT NULL CHECK (revision >= 1),
	updated_at INTEGER NOT NULL
) STRICT;

CREATE TABLE identity_project_roles (
	project_id   TEXT    NOT NULL,
	principal_id TEXT    NOT NULL,
	role         TEXT    NOT NULL CHECK (role IN ('owner', 'coordinator', 'contributor', 'curator', 'checker', 'reviewer', 'viewer')),
	granted_at   INTEGER NOT NULL,
	granted_by   TEXT    NOT NULL,
	PRIMARY KEY (project_id, principal_id, role)
) STRICT;

CREATE INDEX identity_project_roles_by_principal ON identity_project_roles (principal_id);

-- scope 为空串表示实例级覆盖，否则为 project_id；value 为 RFC 8785 规范化 JSON。
CREATE TABLE identity_policies (
	scope      TEXT    NOT NULL,
	key        TEXT    NOT NULL,
	value      TEXT    NOT NULL,
	revision   INTEGER NOT NULL CHECK (revision >= 1),
	updated_at INTEGER NOT NULL,
	updated_by TEXT    NOT NULL,
	PRIMARY KEY (scope, key)
) STRICT;

-- 长期凭据只存 SHA-256 与明文前缀；只能用来换会话。
CREATE TABLE identity_credentials (
	credential_id TEXT    PRIMARY KEY,
	principal_id  TEXT    NOT NULL,
	prefix        TEXT    NOT NULL,
	secret_hash   TEXT    NOT NULL UNIQUE,
	scopes        TEXT    NOT NULL,
	projects      TEXT    NOT NULL DEFAULT '[]',
	label         TEXT    NOT NULL DEFAULT '',
	created_at    INTEGER NOT NULL,
	created_by    TEXT    NOT NULL,
	expires_at    INTEGER NOT NULL,
	revoked_at    INTEGER,
	revoked_by    TEXT,
	revoke_reason TEXT
) STRICT;

CREATE INDEX identity_credentials_by_principal ON identity_credentials (principal_id);

-- 人的认证因子状态：enrolled 可正常登录；setup_pending/reset_pending 只能经
-- 受限会话完成设置，pending_operation_id 绑定进行中的设置或恢复操作。
CREATE TABLE identity_human_accounts (
	principal_id         TEXT    PRIMARY KEY,
	factor_state         TEXT    NOT NULL CHECK (factor_state IN ('enrolled', 'setup_pending', 'reset_pending')),
	pending_operation_id TEXT,
	revision             INTEGER NOT NULL CHECK (revision >= 1),
	updated_at           INTEGER NOT NULL,
	CHECK ((factor_state = 'enrolled') = (pending_operation_id IS NULL))
) STRICT;

CREATE TABLE identity_passwords (
	principal_id TEXT    PRIMARY KEY,
	params       TEXT    NOT NULL,
	salt         BLOB    NOT NULL,
	hash         BLOB    NOT NULL,
	updated_at   INTEGER NOT NULL
) STRICT;

-- TOTP 种子经主密钥 AEAD 加密；last_accepted_counter 保证同一时间步只能成功一次。
CREATE TABLE identity_totp_factors (
	factor_id             TEXT    PRIMARY KEY,
	principal_id          TEXT    NOT NULL,
	key_id                TEXT    NOT NULL,
	sealed_secret         BLOB    NOT NULL,
	algorithm             TEXT    NOT NULL CHECK (algorithm = 'SHA1'),
	digits                INTEGER NOT NULL CHECK (digits = 6),
	period                INTEGER NOT NULL CHECK (period = 30),
	state                 TEXT    NOT NULL CHECK (state IN ('pending', 'enabled', 'disabled')),
	last_accepted_counter INTEGER NOT NULL DEFAULT -1,
	created_at            INTEGER NOT NULL,
	enabled_at            INTEGER,
	disabled_at           INTEGER
) STRICT;

CREATE UNIQUE INDEX identity_totp_one_enabled ON identity_totp_factors (principal_id) WHERE state = 'enabled';
CREATE UNIQUE INDEX identity_totp_one_pending ON identity_totp_factors (principal_id) WHERE state = 'pending';

-- 一次性恢复码只存带盐校验值。
CREATE TABLE identity_recovery_codes (
	code_id               TEXT    PRIMARY KEY,
	principal_id          TEXT    NOT NULL,
	batch_id              TEXT    NOT NULL,
	salt                  BLOB    NOT NULL,
	hash                  BLOB    NOT NULL,
	created_at            INTEGER NOT NULL,
	consumed_at           INTEGER,
	consumed_operation_id TEXT,
	invalidated_at        INTEGER
) STRICT;

CREATE INDEX identity_recovery_codes_by_principal ON identity_recovery_codes (principal_id);

-- 管理员为新成员或重置发放的一次性设置码。
CREATE TABLE identity_setup_codes (
	code_id        TEXT    PRIMARY KEY,
	principal_id   TEXT    NOT NULL,
	operation_id   TEXT    NOT NULL,
	salt           BLOB    NOT NULL,
	hash           BLOB    NOT NULL,
	created_by     TEXT    NOT NULL,
	created_at     INTEGER NOT NULL,
	expires_at     INTEGER NOT NULL,
	consumed_at    INTEGER,
	invalidated_at INTEGER
) STRICT;

CREATE INDEX identity_setup_codes_by_principal ON identity_setup_codes (principal_id);

CREATE TABLE identity_challenges (
	challenge_id      TEXT    PRIMARY KEY,
	human_id          TEXT    NOT NULL,
	session_id        TEXT    NOT NULL,
	action            TEXT    NOT NULL,
	project_id        TEXT    NOT NULL DEFAULT '',
	targets           TEXT    NOT NULL,
	target_set_digest TEXT    NOT NULL,
	request_hash      TEXT    NOT NULL,
	operation_id      TEXT    NOT NULL,
	summary           TEXT    NOT NULL,
	created_at        INTEGER NOT NULL,
	expires_at        INTEGER NOT NULL,
	attempts          INTEGER NOT NULL DEFAULT 0,
	state             TEXT    NOT NULL CHECK (state IN ('pending', 'verified', 'expired', 'locked')),
	grant_id          TEXT
) STRICT;

-- 未提交的操作在授权过期后可以按同一摘要重新挑战，因此一个 operation 可有多个挑战与授权；
-- 业务效果只提交一次由该 operation 的命令回执唯一保证。
CREATE INDEX identity_challenges_by_operation ON identity_challenges (operation_id);

CREATE TABLE identity_human_grants (
	grant_id          TEXT    PRIMARY KEY,
	challenge_id      TEXT    NOT NULL UNIQUE,
	human_id          TEXT    NOT NULL,
	session_id        TEXT    NOT NULL,
	action            TEXT    NOT NULL,
	project_id        TEXT    NOT NULL DEFAULT '',
	target_set_digest TEXT    NOT NULL,
	request_hash      TEXT    NOT NULL,
	operation_id      TEXT    NOT NULL,
	issued_at         INTEGER NOT NULL,
	expires_at        INTEGER NOT NULL,
	auth_epoch        INTEGER NOT NULL,
	recovery_epoch    INTEGER NOT NULL,
	state             TEXT    NOT NULL CHECK (state IN ('bound', 'revoked')),
	revoked_at        INTEGER,
	revoke_reason     TEXT
) STRICT;

CREATE INDEX identity_human_grants_by_human ON identity_human_grants (human_id) WHERE state = 'bound';

-- 管理员经人类授权吊销的会话。会话运行记录在 runtime.db，吊销的权威记录
-- 与命令回执同在 main.db 一个事务中提交；验证会话时两处都要核对。
CREATE TABLE identity_session_revocations (
	session_id   TEXT    PRIMARY KEY,
	principal_id TEXT    NOT NULL,
	revoked_at   INTEGER NOT NULL,
	revoked_by   TEXT    NOT NULL,
	reason       TEXT    NOT NULL,
	operation_id TEXT    NOT NULL
) STRICT;

-- 认证失败按主体与来源分别计数，用于滚动窗口限速；成功不写入。
CREATE TABLE identity_auth_failures (
	seq          INTEGER PRIMARY KEY,
	principal_id TEXT    NOT NULL DEFAULT '',
	source       TEXT    NOT NULL DEFAULT '',
	kind         TEXT    NOT NULL CHECK (kind IN ('login', 'totp', 'recovery', 'setup')),
	at           INTEGER NOT NULL
) STRICT;

CREATE INDEX identity_auth_failures_principal ON identity_auth_failures (principal_id, at);
CREATE INDEX identity_auth_failures_source ON identity_auth_failures (source, at);
