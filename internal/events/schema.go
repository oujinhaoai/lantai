// Package events 收录多源 outbox、提供内部重放与本库事务消费适配，并维护审计和保留水位。
// 不创建后台 goroutine、HTTP 路由或跨库事务；调用者显式调度有界的一步操作。
package events

import (
	"context"
	"github.com/oujinhaoai/lantai/internal/commands"
)

// SchemaSQL 是 events.db 的唯一 DDL，由实例迁移引用。
const SchemaSQL = `
CREATE TABLE IF NOT EXISTS events_records (
 global_seq INTEGER PRIMARY KEY AUTOINCREMENT,
 event_id TEXT NOT NULL UNIQUE,
 envelope TEXT NOT NULL,
 recorded_at INTEGER NOT NULL
) STRICT;
CREATE TABLE IF NOT EXISTS events_identities (
 event_id TEXT PRIMARY KEY,
 global_seq INTEGER NOT NULL UNIQUE,
 envelope_digest TEXT NOT NULL,
 recorded_at INTEGER NOT NULL
) STRICT;
CREATE TABLE IF NOT EXISTS events_retention (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 pruned_through INTEGER NOT NULL DEFAULT 0,
 audit_through INTEGER NOT NULL DEFAULT 0,
 backup_through INTEGER NOT NULL DEFAULT 0
) STRICT;
INSERT OR IGNORE INTO events_retention(singleton) VALUES(1);
CREATE TABLE IF NOT EXISTS events_consumers (
 consumer TEXT PRIMARY KEY,
 through_seq INTEGER NOT NULL DEFAULT 0,
 required INTEGER NOT NULL CHECK(required IN (0,1))
) STRICT;
CREATE TABLE IF NOT EXISTS events_relay_state (
 source TEXT PRIMARY KEY,
 attempts INTEGER NOT NULL DEFAULT 0,
 retry_at INTEGER NOT NULL DEFAULT 0,
 failure_kind TEXT NOT NULL DEFAULT ''
) STRICT;
CREATE TABLE IF NOT EXISTS events_audit_files (
 name TEXT PRIMARY KEY,
 size INTEGER NOT NULL,
 digest TEXT NOT NULL,
 through_seq INTEGER NOT NULL,
 records INTEGER NOT NULL
) STRICT;
`

// ConsumerSchemaSQL 安装在消费者所属库；状态变更、去重和水位共用该库事务。
// 表按 consumer 分区，不允许消费者借此写其他模块的业务表。
const ConsumerSchemaSQL = `
CREATE TABLE IF NOT EXISTS processed_events (
 consumer TEXT NOT NULL,
 event_id TEXT NOT NULL,
 global_seq INTEGER NOT NULL,
 processed_at INTEGER NOT NULL,
 PRIMARY KEY(consumer,event_id)
) STRICT;
CREATE TABLE IF NOT EXISTS consumer_offsets (
 consumer TEXT PRIMARY KEY,
 through_seq INTEGER NOT NULL DEFAULT 0
) STRICT;
CREATE TABLE IF NOT EXISTS consumer_failures (
 consumer TEXT NOT NULL,
 event_id TEXT NOT NULL,
 global_seq INTEGER NOT NULL,
 kind TEXT NOT NULL,
 attempts INTEGER NOT NULL,
 retry_at INTEGER NOT NULL,
 PRIMARY KEY(consumer,event_id)
) STRICT;
CREATE TABLE IF NOT EXISTS pending_commands (
 consumer TEXT NOT NULL,
 event_id TEXT NOT NULL,
 step_key TEXT NOT NULL,
 operation_id TEXT NOT NULL UNIQUE,
 command_type TEXT NOT NULL,
 payload TEXT NOT NULL,
 status TEXT NOT NULL CHECK(status IN ('pending','succeeded','blocked')),
 attempts INTEGER NOT NULL DEFAULT 0,
 retry_at INTEGER NOT NULL DEFAULT 0,
 failure_kind TEXT NOT NULL DEFAULT '',
 receipt TEXT,
 PRIMARY KEY(consumer,event_id,step_key)
) STRICT;
`

// EnsureSchema 仅供测试和独立适配；正式实例通过维护模式的迁移安装。
func EnsureSchema(ctx context.Context, q commands.DBTX) error {
	_, err := q.ExecContext(ctx, SchemaSQL)
	return err
}

// EnsureConsumerSchema 安装本库消费者基础设施，正式实例由迁移调用。
func EnsureConsumerSchema(ctx context.Context, q commands.DBTX) error {
	_, err := q.ExecContext(ctx, ConsumerSchemaSQL)
	return err
}
