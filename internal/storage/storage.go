// Package storage 实现 T02 的存储端：内容寻址的原件库（CAS）、上传会话与分片
// 续传、内容复用授权（BlobGrant）、读取授权（ReadGrant）与逐请求核验的下载、
// 版本文件的幂等安装与隔离、追加式证据记录，以及上传 pin 与到期清理。
//
// storage 只管字节与短期传输授权，不自行宣布版本提交、审定或生命周期：版本
// 是否 committed 由台账（commit.Reader）裁决，安装只产出 installed 证明。
// 控制记录在 runtime.db 中本模块拥有的表里，与命令回执、outbox 同事务写入；
// 字节在数据根下（布局见 Layout），只有服务进程写入。权威文件落位与持久记录
// 经实例写入口（commands.Gate），维护与停止期间被拒；私有暂存与安装组装在
// 最终接受之前完成。大文件传输与哈希不持有 security_guard；同分片、同操作
// 的重试仍以局部互斥串行化，最终授权复核与短提交才取得 security_guard。
//
// 安全边界：知道哈希不等于有权使用内容。未经本操作授权的哈希一律按“需要
// 上传或授权”处理，不泄露内容是否已存在；读取授权绑定本人会话与具体版本
// 文件，每个新的 GET/Range 请求都按当前权限、台账状态与限制复核。
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/rights"
	"github.com/oujinhaoai/lantai/internal/storage/fileop"
)

// Module 是本模块在所有权登记中的名称。
const Module = "storage"

// 本模块使用的授权动作，登记在 identity 的权限矩阵中。
const (
	ActionUpload      authz.Action = "storage.upload"
	ActionReadContent authz.Action = "storage.read_content"
)

// 命令类型。
const (
	CommandCreateUpload = "storage.create_upload"
)

// Config 是存储参数；零值字段取 DefaultConfig 中的值。上传大小与文件数限制
// 可配置，超限在接受上传前以 QUOTA_EXCEEDED 拒绝并给出原因，不静默截断。
type Config struct {
	// PartSize 是大文件的分片大小；SinglePartMax 以内的文件整件一次上传。
	PartSize      int64
	SinglePartMax int64
	// MaxFileBytes、MaxUploadBytes、MaxUploadFiles 限制单个文件、单个上传会话
	// 的总字节与文件数。
	MaxFileBytes   int64
	MaxUploadBytes int64
	MaxUploadFiles int
	// UploadIdleTTL 与 UploadMaxTTL 是上传会话的空闲到期与绝对到期。
	UploadIdleTTL time.Duration
	UploadMaxTTL  time.Duration
	// SourceGrantTTL 是按已授权来源签发的复用授权在未被消费前的有效期。
	SourceGrantTTL time.Duration
	// ReadGrantTTL 是读取授权的默认有效期（不超过会话有效期）。
	ReadGrantTTL time.Duration
	// MinFreeBytes 是写入暂存与安装前必须保留的最低可用空间。
	MinFreeBytes uint64
	// MaxManifestBytes 限制版本清单文件的大小。
	MaxManifestBytes int
}

// DefaultConfig 返回设计基线中的默认值（上传空闲 24 小时、绝对 7 天、
// 100 MB 以上分片、读取授权 15 分钟）。
func DefaultConfig() Config {
	return Config{
		PartSize:         64 << 20,
		SinglePartMax:    100 << 20,
		MaxFileBytes:     1 << 40,
		MaxUploadBytes:   4 << 40,
		MaxUploadFiles:   100000,
		UploadIdleTTL:    24 * time.Hour,
		UploadMaxTTL:     7 * 24 * time.Hour,
		SourceGrantTTL:   24 * time.Hour,
		ReadGrantTTL:     15 * time.Minute,
		MinFreeBytes:     1 << 30,
		MaxManifestBytes: 16 << 20,
	}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	pick64 := func(v *int64, def int64) {
		if *v <= 0 {
			*v = def
		}
	}
	pickD := func(v *time.Duration, def time.Duration) {
		if *v <= 0 {
			*v = def
		}
	}
	pick64(&c.PartSize, d.PartSize)
	pick64(&c.SinglePartMax, d.SinglePartMax)
	pick64(&c.MaxFileBytes, d.MaxFileBytes)
	pick64(&c.MaxUploadBytes, d.MaxUploadBytes)
	if c.MaxUploadFiles <= 0 {
		c.MaxUploadFiles = d.MaxUploadFiles
	}
	pickD(&c.UploadIdleTTL, d.UploadIdleTTL)
	pickD(&c.UploadMaxTTL, d.UploadMaxTTL)
	pickD(&c.SourceGrantTTL, d.SourceGrantTTL)
	pickD(&c.ReadGrantTTL, d.ReadGrantTTL)
	if c.MinFreeBytes == 0 {
		c.MinFreeBytes = d.MinFreeBytes
	}
	if c.MaxManifestBytes <= 0 {
		c.MaxManifestBytes = d.MaxManifestBytes
	}
	if c.UploadIdleTTL > c.UploadMaxTTL {
		c.UploadIdleTTL = c.UploadMaxTTL
	}
	return c
}

// ReadGate 是下载开始前的最终读取检查（identity.Service.BeginRead）：在
// security_guard 读锁内按当前权威状态授权后调用 open；锁内只打开，不传输。
type ReadGate interface {
	BeginRead(ctx context.Context, who authz.Context, action authz.Action, res authz.Resource,
		open func(ctx context.Context, d authz.Decision) error) error
}

// Deps 是存储模块依赖的实例资源与其他模块接口，由组装方提供。
type Deps struct {
	// Runtime 是 runtime.db；Home 是数据根目录（绝对路径）。
	Runtime *sql.DB
	Home    string
	// Gate 是实例写入口：全部写入经它进入，维护与停止期间被拒。
	Gate  *commands.Gate
	Clock clock.Clock
	IDs   *ids.Generator
	// Authz 给出当前授权判定；Reads 是下载的最终读取检查。
	Authz authz.Authorizer
	Reads ReadGate
	// Ledger 是已提交版本的权威读取（台账）；Rights 是用途与来源限制。
	Ledger commit.Reader
	Rights rights.Evaluator
	// ReadGrantKey 是签署读取授权的子密钥（由主密钥派生，至少 32 字节）；
	// ReadGrantKeyID 标识它，便于将来轮换。
	ReadGrantKey   []byte
	ReadGrantKeyID string
	InstanceID     ids.ID
	// FS 可注入文件系统故障，只供测试使用。
	FS fileop.FS
}

// Service 是存储模块的应用服务。
type Service struct {
	db       *sql.DB
	layout   Layout
	gate     *commands.Gate
	clock    clock.Clock
	ids      *ids.Generator
	authz    authz.Authorizer
	reads    ReadGate
	ledger   commit.Reader
	rights   rights.Evaluator
	key      []byte
	keyID    string
	instance ids.ID
	fs       fileop.FS
	cfg      Config
	store    *commands.Store

	keyMu    sync.Mutex
	keyLocks map[string]*keyLock
}

// keyLock 是按键的进程内互斥锁，引用计数归零时回收，长期运行不累积。
type keyLock struct {
	mu   sync.Mutex
	refs int
}

// New 创建服务；迁移须已应用到 runtime.db。
func New(d Deps, cfg Config) (*Service, error) {
	switch {
	case d.Runtime == nil || d.Gate == nil:
		return nil, errors.New("storage: runtime database and gate are required")
	case d.Authz == nil || d.Reads == nil || d.Ledger == nil || d.Rights == nil:
		return nil, errors.New("storage: authorizer, read gate, ledger reader and rights evaluator are required")
	case len(d.ReadGrantKey) < 32:
		return nil, errors.New("storage: read grant key must be at least 32 bytes")
	case d.IDs == nil:
		return nil, errors.New("storage: id generator is required")
	case !filepath.IsAbs(d.Home):
		return nil, fmt.Errorf("storage: data root %q must be an absolute path", d.Home)
	}
	if d.Clock == nil {
		d.Clock = clock.System{}
	}
	if d.ReadGrantKeyID == "" {
		d.ReadGrantKeyID = "read-grant/v1"
	}
	st, err := commands.NewStore(Module, d.Clock)
	if err != nil {
		return nil, err
	}
	return &Service{
		db: d.Runtime, layout: Layout{Home: filepath.Clean(d.Home)}, gate: d.Gate, clock: d.Clock, ids: d.IDs,
		authz: d.Authz, reads: d.Reads, ledger: d.Ledger, rights: d.Rights,
		key: append([]byte(nil), d.ReadGrantKey...), keyID: d.ReadGrantKeyID, instance: d.InstanceID,
		fs: d.FS, cfg: cfg.withDefaults(), store: st, keyLocks: map[string]*keyLock{},
	}, nil
}

// Config 返回生效的参数。
func (s *Service) Config() Config { return s.cfg }

// Layout 返回数据目录布局。
func (s *Service) Layout() Layout { return s.layout }

func (s *Service) now() time.Time { return clock.Truncate(s.clock.Now()) }

// write 经实例写入口取得屏障共享锁与 req 中的其余锁。
func (s *Service) write(ctx context.Context, req commands.Request) (context.Context, func(), error) {
	lctx, h, err := s.gate.Acquire(ctx, req)
	if err != nil {
		return ctx, nil, err
	}
	return lctx, h.Release, nil
}

// inTx 在 runtime.db 上执行一个写事务（BEGIN IMMEDIATE）；fn 返回错误时回滚。
func (s *Service) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// authorize 按当前权威状态判定，拒绝转换为结构化错误。
func (s *Service) authorize(ctx context.Context, who authz.Context, action authz.Action, res authz.Resource) (authz.Decision, error) {
	d, err := s.authz.Authorize(ctx, who, action, res)
	if err != nil {
		return d, err
	}
	return d, d.Err()
}

func (s *Service) event(typ, aggType string, aggID ids.ID, revision int64, who authz.Context, project, op ids.ID, payload any) (event.Envelope, error) {
	return event.New(s.ids, s.clock, event.Params{
		EventType: typ, SchemaVersion: 1, AggregateType: aggType, AggregateID: aggID, AggregateRevision: revision,
		ActorID: who.PrincipalID, SessionID: who.SessionID, ProjectID: project, OperationID: op, Payload: payload,
	})
}

func (s *Service) newID() (ids.ID, error) { return s.ids.New() }

// lockKey 取得按键的进程内互斥锁（同一分片、同一安装操作只有一个写入者），
// 返回解锁函数。
func (s *Service) lockKey(key string) (unlock func()) {
	s.keyMu.Lock()
	l := s.keyLocks[key]
	if l == nil {
		l = &keyLock{}
		s.keyLocks[key] = l
	}
	l.refs++
	s.keyMu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		s.keyMu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(s.keyLocks, key)
		}
		s.keyMu.Unlock()
	}
}

// 事件类型；payload 只含状态、ID 与计数，不含路径以外的内容或任何凭据。
const (
	EvUploadCreated      = "upload.created"
	EvUploadFileVerified = "upload.file_verified"
	EvUploadClosed       = "upload.closed"
	EvBlobGrantIssued    = "blob_grant.issued"
)

func invalid(format string, args ...any) *errcode.Error {
	return errcode.Newf(errcode.SchemaInvalid, format, args...)
}

func reasonErr(code errcode.Code, reason, message string, data map[string]any) *errcode.Error {
	return errcode.New(code, message).WithDetails(errcode.Detail{Reason: reason, Message: message, Data: data})
}

// Layout 是 storage 管理的数据目录布局。数据根必须位于服务端本机磁盘上的同一
// 文件系统，以便硬链接去重与同卷改名。
//
//	blobs/sha256/ab/cd/<sha256>                      内容库（只读原件）
//	staging/uploads/<upload_id>/<sha256>.part         上传暂存
//	staging/install/<operation_id>/                   私有安装区
//	projects/<project_id>/assets/<asset_id>/versions/vNNN/
//	    manifest.yaml、files/<相对路径>、.install.json    不可变版本目录
//	projects/<project_id>/assets/<asset_id>/records/<version_id>/<record_id>.json
//	quarantine/<operation_id>/                        隔离区
type Layout struct {
	Home string
}

// BlobPath 返回原件在内容库中的路径。
func (l Layout) BlobPath(sha string) string {
	return filepath.Join(l.Home, "blobs", "sha256", sha[:2], sha[2:4], sha)
}

func (l Layout) uploadDir(upload ids.ID) string {
	return filepath.Join(l.Home, "staging", "uploads", string(upload))
}

func (l Layout) uploadData(upload ids.ID, sha string) string {
	return filepath.Join(l.uploadDir(upload), sha+".part")
}

func (l Layout) installStaging(op ids.ID) string {
	return filepath.Join(l.Home, "staging", "install", string(op))
}

// AssetDir 返回资产目录；catalog 的说明文件与 storage 的版本目录都在其下。
func (l Layout) AssetDir(project, asset ids.ID) string {
	return filepath.Join(l.Home, "projects", string(project), "assets", string(asset))
}

// ProjectDir 返回项目目录。
func (l Layout) ProjectDir(project ids.ID) string {
	return filepath.Join(l.Home, "projects", string(project))
}

// VersionDirName 返回版本目录名（v001、v002…，超过三位按实际位数）。
func VersionDirName(number int64) string { return fmt.Sprintf("v%03d", number) }

// VersionDir 返回版本目录。
func (l Layout) VersionDir(project, asset ids.ID, number int64) string {
	return filepath.Join(l.AssetDir(project, asset), "versions", VersionDirName(number))
}

func (l Layout) recordsDir(project, asset, version ids.ID) string {
	return filepath.Join(l.AssetDir(project, asset), "records", string(version))
}

func (l Layout) quarantineDir(op ids.ID) string {
	return filepath.Join(l.Home, "quarantine", string(op))
}

// ref 把数据根下的路径转为 storage 内部引用（小写、/ 分隔，不是主机路径）。
func (l Layout) ref(path string) string {
	rel, err := filepath.Rel(l.Home, path)
	if err != nil {
		return ""
	}
	return strings.ToLower(filepath.ToSlash(rel))
}
