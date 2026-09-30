// Package query 实现可删除重建的目录、文本与关联投影。index.db 不是业务
// 真源；读取在响应时核对当前授权与来源限制，精确读取继续直接走 catalog。
package query

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/contract/rights"
	"github.com/oujinhaoai/lantai/internal/events"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite/migrations"
)

const Module = "query"

// Catalog 是 T02 提供的可信文件读取；该接口不对终端调用者开放。
type Catalog interface {
	ReadProjection(context.Context, ids.ID) (catalog.AssetInfo, error)
	ReadProjectionVersion(context.Context, ids.ID, ids.ID) (catalog.VersionInfo, error)
}

// EventLog 由 events.Store 实现。Read.HighWater 是已扫描高水位，即使本页
// 的事件全部被权限过滤，调用方也必须前移；过保留窗口返回 CURSOR_EXPIRED。
type EventLog interface {
	HighWater(context.Context) (int64, error)
	Read(context.Context, int64, int) (events.Page, error)
}

// Retainer 是真实 events.Store 的关键消费者保留接口。纯读取测试桩可不实现。
// 注册先于扫描，确认只能在 index 本库事务成功之后进行，不能共用跨库事务。
type Retainer interface {
	RegisterConsumer(context.Context, string, bool) error
	AcknowledgeConsumer(context.Context, string, int64) error
}

// RetentionPendingError 明确表示 index 已提交，只有 events 保留水位确认失败。
// 重试 CatchUp（即使没有新事件）会补确认，不需要重新构建已发布投影。
type RetentionPendingError struct {
	Committed State
	Cause     error
}

func (e *RetentionPendingError) Error() string {
	return fmt.Sprintf("query: index committed through %d; retention acknowledgement pending; retry CatchUp: %v", e.Committed.HighWater, e.Cause)
}
func (e *RetentionPendingError) Unwrap() error { return e.Cause }
func (s *Service) registerRetention(ctx context.Context) error {
	if r, ok := s.events.(Retainer); ok {
		return r.RegisterConsumer(ctx, Module, true)
	}
	return nil
}
func (s *Service) acknowledgeRetention(ctx context.Context, st State) error {
	if r, ok := s.events.(Retainer); ok {
		if err := r.AcknowledgeConsumer(ctx, Module, st.HighWater); err != nil {
			return &RetentionPendingError{Committed: st, Cause: err}
		}
	}
	return nil
}

// Deps 只接收各模块公开的核心接口，不跨库读取对方的 SQL 表。
type Deps struct {
	DB         *sql.DB
	Gate       *commands.Gate
	Reader     commit.Reader
	Catalog    Catalog
	Events     EventLog
	Authz      authz.Authorizer
	Rights     rights.Evaluator
	InstanceID ids.ID
}

type Service struct {
	db       *sql.DB
	gate     *commands.Gate
	reader   commit.Reader
	catalog  Catalog
	events   EventLog
	authz    authz.Authorizer
	rights   rights.Evaluator
	instance ids.ID
	// build 保证单核心进程内只有一个构建/增量写入者；mu 只在提交 index 写事务
	// 时持有，读者在其内读取，看到的是原子切换后的整代或某个已提交页边界。
	build  sync.Mutex
	mu     sync.Mutex
	cursor cipher.AEAD
}

func New(d Deps) (*Service, error) {
	if d.DB == nil || d.Gate == nil || d.Reader == nil || d.Catalog == nil || d.Events == nil || d.Authz == nil || d.Rights == nil || !d.InstanceID.Valid() {
		return nil, errors.New("query: database, gate, authoritative readers, events, authorization, rights and instance are required")
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Service{db: d.DB, gate: d.Gate, reader: d.Reader, catalog: d.Catalog, events: d.Events, authz: d.Authz, rights: d.Rights, instance: d.InstanceID, cursor: aead}, nil
}

// EnsureSchema 仅供空的独立 index 库/测试初始化；实例迁移由 operations 管理。
func EnsureSchema(ctx context.Context, db *sql.DB) error {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='query_state'`).Scan(&n); err != nil {
		return err
	}
	if n != 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, m := range migrations.For(ownership.Index) {
		if m.Owner == Module {
			if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// State 是已发布投影的代次与水位。Generation=0 表示尚未完成全量构建。
type State struct {
	Generation   int64 `json:"generation"`
	HighWater    int64 `json:"high_water"`
	RebuildStart int64 `json:"rebuild_start"`
}

func readState(ctx context.Context, q commands.DBTX) (State, error) {
	var st State
	err := q.QueryRowContext(ctx, `SELECT generation, high_water, rebuild_start FROM query_state WHERE singleton=1`).Scan(&st.Generation, &st.HighWater, &st.RebuildStart)
	return st, err
}
func (s *Service) State(ctx context.Context) (State, error) { return readState(ctx, s.db) }

// Item 是安全的目录摘要，不含 uses、路径细节、许可证据或自由格式来源元数据。
type Item struct {
	AssetID       ids.ID             `json:"asset_id"`
	ProjectID     ids.ID             `json:"project_id"`
	VersionID     ids.ID             `json:"version_id"`
	VersionNumber int64              `json:"version_number"`
	AssetType     manifest.AssetType `json:"asset_type"`
	Slug          string             `json:"slug"`
	Title         string             `json:"title"`
	Summary       string             `json:"summary,omitempty"`
	Tags          []string           `json:"tags"`
	Subjects      []string           `json:"subjects"`
	Revision      int64              `json:"revision"`
}

func itemOf(a catalog.AssetInfo) Item {
	return Item{AssetID: a.Asset.AssetID, ProjectID: a.Asset.ProjectID, VersionID: a.Latest.VersionID, VersionNumber: a.Latest.VersionNumber, AssetType: a.Description.AssetType, Slug: a.Description.Slug, Title: a.Description.Title, Summary: a.Description.Summary, Tags: a.Description.Tags, Subjects: a.Description.Subjects, Revision: a.Description.Revision}
}

func expired(message string) error { return errcode.New(errcode.CursorExpired, message) }
