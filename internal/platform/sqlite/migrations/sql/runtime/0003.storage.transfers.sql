-- storage（T02）在 runtime.db 的记录：上传会话与分片、内容复用授权（BlobGrant）、
-- 读取授权（ReadGrant）、版本安装记录与版本文件清单、上传 pin。字节本身在
-- 数据根的 blobs/ 与 staging/ 下；这里只放控制记录。已提交版本的 Blob 引用
-- 由台账的提交记录与不可变 manifest 维持，不依赖这里的短期授权。

-- 上传会话是一次提交操作的 receiving 阶段：operation_id 由服务端分配，之后
-- 用作版本提交的 operation。有效期取空闲到期与绝对到期中较早者。
CREATE TABLE storage_uploads (
	upload_id       TEXT    PRIMARY KEY,
	operation_id    TEXT    NOT NULL UNIQUE,
	principal_id    TEXT    NOT NULL,
	session_id      TEXT    NOT NULL,
	project_id      TEXT    NOT NULL,
	state           TEXT    NOT NULL CHECK (state IN ('open', 'completed', 'expired', 'cancelled')),
	file_count      INTEGER NOT NULL CHECK (file_count >= 0),
	total_bytes     INTEGER NOT NULL CHECK (total_bytes >= 0),
	created_at      INTEGER NOT NULL,
	last_activity   INTEGER NOT NULL,
	idle_expires_at INTEGER NOT NULL,
	expires_at      INTEGER NOT NULL,
	closed_at       INTEGER,
	close_reason    TEXT,
	-- revision 是会话聚合的修订，随每个事件递增（事件信封的 aggregate_revision）。
	revision        INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1),
	CHECK ((state = 'open') = (closed_at IS NULL))
) STRICT;

CREATE INDEX storage_uploads_open ON storage_uploads (idle_expires_at) WHERE state = 'open';

-- 会话申报的文件：按内容（sha256）去重，同一内容只上传一次。
CREATE TABLE storage_upload_files (
	upload_id   TEXT    NOT NULL,
	sha256      TEXT    NOT NULL,
	size        INTEGER NOT NULL CHECK (size >= 0),
	part_size   INTEGER NOT NULL CHECK (part_size >= 1),
	part_count  INTEGER NOT NULL CHECK (part_count >= 1),
	state       TEXT    NOT NULL CHECK (state IN ('pending', 'verified', 'failed')),
	verified_at INTEGER,
	PRIMARY KEY (upload_id, sha256)
) STRICT;

-- 已写入暂存区并逐片核验过的分片；只有数据刷盘后才记录。
CREATE TABLE storage_upload_parts (
	upload_id    TEXT    NOT NULL,
	sha256       TEXT    NOT NULL,
	part_number  INTEGER NOT NULL CHECK (part_number >= 1),
	start_offset INTEGER NOT NULL CHECK (start_offset >= 0),
	size         INTEGER NOT NULL CHECK (size >= 0),
	part_sha256  TEXT    NOT NULL,
	received_at  INTEGER NOT NULL,
	PRIMARY KEY (upload_id, sha256, part_number)
) STRICT;

-- 内容复用授权：只允许指定项目中的指定操作使用这份字节，不授予按哈希下载。
-- uploaded 表示本人完整上传并经服务端核验；authorized_source 表示调用者对
-- 来源版本的文件有读取与用途授权。被操作消费后（consumed_at）不再受到期影响。
CREATE TABLE storage_blob_grants (
	grant_id          TEXT    PRIMARY KEY,
	principal_id      TEXT    NOT NULL,
	session_id        TEXT    NOT NULL,
	project_id        TEXT    NOT NULL,
	operation_id      TEXT    NOT NULL,
	sha256            TEXT    NOT NULL,
	size              INTEGER NOT NULL CHECK (size >= 0),
	basis             TEXT    NOT NULL CHECK (basis IN ('uploaded', 'authorized_source')),
	source_project_id TEXT,
	source_asset_id   TEXT,
	source_version_id TEXT,
	source_path       TEXT,
	purpose           TEXT    NOT NULL,
	issued_at         INTEGER NOT NULL,
	expires_at        INTEGER NOT NULL,
	consumed_at       INTEGER,
	revoked_at        INTEGER,
	revoke_reason     TEXT,
	CHECK ((basis = 'authorized_source') = (source_version_id IS NOT NULL))
) STRICT;

CREATE INDEX storage_blob_grants_by_operation ON storage_blob_grants (operation_id, sha256);
CREATE UNIQUE INDEX storage_blob_grants_one_per_basis ON storage_blob_grants (operation_id, sha256, basis, IFNULL(source_version_id, ''), IFNULL(source_path, ''), purpose);

-- 读取授权：绑定主体与本人会话、项目、版本、文件、哈希、方法与用途；每个新的
-- GET/Range 请求仍按当前权限、生命周期与限制复核，撤销后立即拒绝。
CREATE TABLE storage_read_grants (
	grant_id       TEXT    PRIMARY KEY,
	principal_id   TEXT    NOT NULL,
	session_id     TEXT    NOT NULL,
	project_id     TEXT    NOT NULL,
	asset_id       TEXT    NOT NULL,
	version_id     TEXT    NOT NULL,
	path           TEXT    NOT NULL,
	sha256         TEXT    NOT NULL,
	size           INTEGER NOT NULL CHECK (size >= 0),
	method         TEXT    NOT NULL CHECK (method = 'GET'),
	purpose        TEXT    NOT NULL,
	issued_at      INTEGER NOT NULL,
	expires_at     INTEGER NOT NULL,
	signing_key_id TEXT    NOT NULL,
	auth_epoch     INTEGER NOT NULL,
	rights_epoch   INTEGER NOT NULL,
	revoked_at     INTEGER,
	revoke_reason  TEXT
) STRICT;

CREATE INDEX storage_read_grants_by_session ON storage_read_grants (session_id) WHERE revoked_at IS NULL;

-- 版本文件安装记录：同一 operation 只安装一次，安装内容在台账 committed 前不可读。
-- 隔离的操作保留字节供诊断，不能再次安装；未曾安装就被隔离的操作只有状态。
CREATE TABLE storage_installs (
	operation_id      TEXT    PRIMARY KEY,
	state             TEXT    NOT NULL CHECK (state IN ('installed', 'quarantined')),
	project_id        TEXT,
	asset_id          TEXT,
	version_id        TEXT    UNIQUE,
	version_number    INTEGER,
	manifest_digest   TEXT,
	manifest_sha256   TEXT,
	request_digest    TEXT,
	install_ref       TEXT,
	link_mode         TEXT    CHECK (link_mode IN ('hardlink', 'copy', 'mixed')),
	installed_at      INTEGER,
	quarantine_reason TEXT,
	quarantine_ref    TEXT,
	quarantined_at    INTEGER,
	UNIQUE (asset_id, version_number),
	CHECK ((state = 'quarantined') = (quarantined_at IS NOT NULL))
) STRICT;

-- 已安装版本的冻结文件清单（与 manifest 一致），用于按版本与路径授权读取；
-- 按哈希的索引供将来的 GC 核对引用。
CREATE TABLE storage_version_files (
	version_id TEXT    NOT NULL,
	path       TEXT    NOT NULL,
	sha256     TEXT    NOT NULL,
	size       INTEGER NOT NULL CHECK (size >= 0),
	PRIMARY KEY (version_id, path)
) STRICT;

CREATE INDEX storage_version_files_by_blob ON storage_version_files (sha256);

-- upload pin：上传会话持有的内容保留记录，随会话到期或关闭释放；提交 pin 归
-- 台账、备份 pin 归运维，互不影响。
CREATE TABLE storage_pins (
	pin_id      TEXT    PRIMARY KEY,
	owner_kind  TEXT    NOT NULL CHECK (owner_kind = 'upload_session'),
	owner_id    TEXT    NOT NULL UNIQUE,
	created_at  INTEGER NOT NULL,
	expires_at  INTEGER NOT NULL,
	released_at INTEGER
) STRICT;

CREATE TABLE storage_pin_blobs (
	pin_id TEXT NOT NULL,
	sha256 TEXT NOT NULL,
	PRIMARY KEY (pin_id, sha256)
) STRICT;

CREATE INDEX storage_pin_blobs_by_blob ON storage_pin_blobs (sha256);
