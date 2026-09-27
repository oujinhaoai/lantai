package commands

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite"
)

// SchemaSQL 创建每个业务库内的基础设施表。main、ledger、runtime 各自的迁移
// 包含这段 DDL；表属于共享的命令组件，行按 owner_module 归属各模块。
// events 与 index 库不使用这三张表。
const SchemaSQL = `
CREATE TABLE IF NOT EXISTS command_receipts (
	actor_id            TEXT    NOT NULL,
	project_scope       TEXT    NOT NULL,
	command_type        TEXT    NOT NULL,
	idempotency_key     TEXT    NOT NULL,
	operation_id        TEXT    NOT NULL UNIQUE,
	owner_module        TEXT    NOT NULL,
	request_hash        TEXT    NOT NULL,
	status              TEXT    NOT NULL CHECK (status IN ('in_progress', 'succeeded', 'failed')),
	result_refs         TEXT    NOT NULL DEFAULT '[]',
	response_code       INTEGER,
	response_summary    TEXT,
	failure_code        TEXT,
	created_at          INTEGER NOT NULL,
	completed_at        INTEGER,
	response_expires_at INTEGER,
	PRIMARY KEY (actor_id, project_scope, command_type, idempotency_key)
) STRICT;

CREATE TABLE IF NOT EXISTS operations (
	operation_id          TEXT    PRIMARY KEY,
	parent_operation_id   TEXT,
	owner_module          TEXT    NOT NULL,
	command_type          TEXT    NOT NULL,
	request_hash          TEXT    NOT NULL,
	target_ids            TEXT    NOT NULL DEFAULT '[]',
	expected_revisions    TEXT    NOT NULL DEFAULT '{}',
	auth_revision         INTEGER NOT NULL DEFAULT 0,
	recovery_epoch        INTEGER NOT NULL,
	stage                 TEXT    NOT NULL,
	manifest_digest       TEXT,
	intended_paths        TEXT,
	immutable_payload_ref TEXT,
	failure_code          TEXT,
	created_at            INTEGER NOT NULL,
	updated_at            INTEGER NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS operations_open ON operations(stage)
	WHERE stage NOT IN ('committed', 'failed', 'cancelled');

CREATE TABLE IF NOT EXISTS outbox (
	seq                INTEGER PRIMARY KEY AUTOINCREMENT,
	event_id           TEXT    NOT NULL UNIQUE,
	operation_id       TEXT    NOT NULL,
	aggregate_id       TEXT    NOT NULL,
	aggregate_revision INTEGER NOT NULL,
	event_type         TEXT    NOT NULL,
	schema_version     INTEGER NOT NULL,
	envelope           TEXT    NOT NULL,
	occurred_at        INTEGER NOT NULL,
	delivered_at       INTEGER
) STRICT;

CREATE INDEX IF NOT EXISTS outbox_pending ON outbox(seq) WHERE delivered_at IS NULL;
CREATE INDEX IF NOT EXISTS outbox_by_operation ON outbox(operation_id);
`

// ResponseTTL 是完整响应的缓存期；过期后回执与去重事实仍保留，
// 只是不再原样重放结果（IDEMPOTENCY_RESULT_EXPIRED）。
const ResponseTTL = 24 * time.Hour

// MaxSummaryBytes 限制回执中保存的响应摘要大小。
const MaxSummaryBytes = 64 << 10

// DBTX 是 *sql.DB、*sql.Tx、*sql.Conn 的公共方法集。
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// EnsureSchema 创建基础设施表；正式实例由各库迁移负责，这里供测试与桩使用。
func EnsureSchema(ctx context.Context, db DBTX) error {
	_, err := db.ExecContext(ctx, SchemaSQL)
	return err
}

// ReceiptKey 是幂等键作用域。
type ReceiptKey struct {
	ActorID        ids.ID
	ProjectID      ids.ID // 实例级命令为空
	CommandType    string
	IdempotencyKey string
}

// ReceiptStatus 是回执状态。
type ReceiptStatus string

const (
	ReceiptInProgress ReceiptStatus = "in_progress"
	ReceiptSucceeded  ReceiptStatus = "succeeded"
	ReceiptFailed     ReceiptStatus = "failed" // 已接受的业务失败，有终态回执
)

// ResultRef 是操作结果对象的类型化引用。
type ResultRef struct {
	Kind     string `json:"kind"`
	ID       ids.ID `json:"id"`
	Revision int64  `json:"revision,omitempty"`
	URI      string `json:"uri,omitempty"`
}

// Receipt 是命令回执。
type Receipt struct {
	Key               ReceiptKey
	OperationID       ids.ID
	OwnerModule       string
	RequestHash       digest.Digest
	Status            ReceiptStatus
	ResultRefs        []ResultRef
	ResponseCode      int
	ResponseSummary   json.RawMessage
	FailureCode       errcode.Code
	CreatedAt         time.Time
	CompletedAt       time.Time
	ResponseExpiresAt time.Time
}

// Outcome 是同一幂等键再次到达时的判定。
type Outcome int

const (
	// OutcomeExecute 表示没有回执，应执行命令。
	OutcomeExecute Outcome = iota
	// OutcomeReplay 表示同键同摘要且已终结，返回保存的结果；调用方必须先按当前读取权限复核。
	OutcomeReplay
	// OutcomeInProgress 表示同键同摘要且仍在进行，返回 202 与同一 operation_id。
	OutcomeInProgress
	// OutcomeConflict 表示同键不同摘要：IDEMPOTENCY_CONFLICT。
	OutcomeConflict
	// OutcomeExpired 表示结果已超过响应缓存期：IDEMPOTENCY_RESULT_EXPIRED，不当新请求执行。
	OutcomeExpired
)

func (o Outcome) String() string {
	return [...]string{"execute", "replay", "in_progress", "conflict", "expired"}[o]
}

// Decide 实现提交协议的幂等判定表。
func Decide(r *Receipt, requestHash digest.Digest, now time.Time) Outcome {
	switch {
	case r == nil:
		return OutcomeExecute
	case r.RequestHash != requestHash:
		return OutcomeConflict
	case r.Status == ReceiptInProgress:
		return OutcomeInProgress
	case !now.Before(r.ResponseExpiresAt):
		return OutcomeExpired
	default:
		return OutcomeReplay
	}
}

// Err 返回判定对应的结构化错误；Execute、Replay、InProgress 返回 nil。
// 过期错误不附带结果引用，调用方按当前读取权限过滤后自行附加。
func (o Outcome) Err(r *Receipt) *errcode.Error {
	switch o {
	case OutcomeConflict:
		return errcode.New(errcode.IdempotencyConflict, "idempotency key was already used for a different request").
			WithOperation(string(r.OperationID)).
			WithHint("use a new Idempotency-Key for a new intent")
	case OutcomeExpired:
		// 结果引用可能已不再可见：由调用方按当前读取权限过滤 r.ResultRefs 后再附加。
		return errcode.New(errcode.IdempotencyResultExpired, "the original result is past its response cache window").
			WithOperation(string(r.OperationID))
	}
	return nil
}

// Store 是一个模块在自己数据库里使用的回执/操作/outbox 组件。
// 它只写这三张基础设施表，并只推进 owner_module 为本模块的记录。
//
// 传入的 *sql.DB 必须由 platform/sqlite.Open 打开：写事务以 BEGIN IMMEDIATE
// 开始，同键并发请求会排队后按回执判定；若用普通 BEGIN，读锁升级为写锁时
// 可能直接得到 busy 错误。
type Store struct {
	module string
	clock  clock.Clock
}

// NewStore 为模块创建组件。
func NewStore(module string, clk clock.Clock) (*Store, error) {
	if !moduleRE.MatchString(module) {
		return nil, fmt.Errorf("commands: invalid module name %q", module)
	}
	if clk == nil {
		clk = clock.System{}
	}
	return &Store{module: module, clock: clk}, nil
}

// Module 返回组件所属模块。
func (s *Store) Module() string { return s.module }

var (
	// ErrNotFound 表示记录不存在。
	ErrNotFound = errors.New("commands: not found")
	// ErrStageConflict 表示阶段转移的前置阶段已不是预期值。
	ErrStageConflict = errors.New("commands: operation stage changed concurrently or transition not allowed")
	// ErrForeignCommand 表示命令或操作不属于本模块。
	ErrForeignCommand = errors.New("commands: command belongs to another module")
)

func (s *Store) owns(commandType string) bool {
	return strings.HasPrefix(commandType, s.module+".")
}

// LookupReceipt 按作用域查找回执；不存在时返回 nil, nil。
func (s *Store) LookupReceipt(ctx context.Context, q DBTX, key ReceiptKey) (*Receipt, error) {
	row := q.QueryRowContext(ctx, `SELECT operation_id, owner_module, request_hash, status, result_refs,
		response_code, response_summary, failure_code, created_at, completed_at, response_expires_at
		FROM command_receipts WHERE actor_id = ? AND project_scope = ? AND command_type = ? AND idempotency_key = ?`,
		key.ActorID, key.ProjectID, key.CommandType, key.IdempotencyKey)
	r, err := scanReceipt(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Key = key
	return r, nil
}

// ReceiptByOperation 按 operation_id 查找回执。
func (s *Store) ReceiptByOperation(ctx context.Context, q DBTX, opID ids.ID) (*Receipt, error) {
	row := q.QueryRowContext(ctx, `SELECT operation_id, owner_module, request_hash, status, result_refs,
		response_code, response_summary, failure_code, created_at, completed_at, response_expires_at,
		actor_id, project_scope, command_type, idempotency_key
		FROM command_receipts WHERE operation_id = ?`, opID)
	var r Receipt
	var refs string
	var code sql.NullInt64
	var summary, failure sql.NullString
	var created int64
	var completed, expires sql.NullInt64
	err := row.Scan(&r.OperationID, &r.OwnerModule, &r.RequestHash, &r.Status, &refs, &code, &summary, &failure,
		&created, &completed, &expires, &r.Key.ActorID, &r.Key.ProjectID, &r.Key.CommandType, &r.Key.IdempotencyKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := fillReceipt(&r, refs, code, summary, failure, created, completed, expires); err != nil {
		return nil, err
	}
	return &r, nil
}

type rowScanner interface{ Scan(dest ...any) error }

func scanReceipt(row rowScanner) (*Receipt, error) {
	var r Receipt
	var refs string
	var code sql.NullInt64
	var summary, failure sql.NullString
	var created int64
	var completed, expires sql.NullInt64
	if err := row.Scan(&r.OperationID, &r.OwnerModule, &r.RequestHash, &r.Status, &refs, &code, &summary, &failure,
		&created, &completed, &expires); err != nil {
		return nil, err
	}
	if err := fillReceipt(&r, refs, code, summary, failure, created, completed, expires); err != nil {
		return nil, err
	}
	return &r, nil
}

func fillReceipt(r *Receipt, refs string, code sql.NullInt64, summary, failure sql.NullString, created int64, completed, expires sql.NullInt64) error {
	if err := json.Unmarshal([]byte(refs), &r.ResultRefs); err != nil {
		return fmt.Errorf("commands: corrupt result_refs for %s: %w", r.OperationID, err)
	}
	r.ResponseCode = int(code.Int64)
	if summary.Valid {
		r.ResponseSummary = json.RawMessage(summary.String)
	}
	r.FailureCode = errcode.Code(failure.String)
	r.CreatedAt = clock.FromMillis(created)
	if completed.Valid {
		r.CompletedAt = clock.FromMillis(completed.Int64)
	}
	if expires.Valid {
		r.ResponseExpiresAt = clock.FromMillis(expires.Int64)
	}
	return nil
}

func (s *Store) insertReceipt(ctx context.Context, q DBTX, cmd Context, status ReceiptStatus, res *Result, now time.Time) (*Receipt, error) {
	r := &Receipt{
		Key:         cmd.Key(),
		OperationID: cmd.OperationID,
		OwnerModule: s.module,
		RequestHash: cmd.RequestHash,
		Status:      status,
		CreatedAt:   now,
	}
	var code, completed, expires any
	var summary, failure any
	refs := "[]"
	if res != nil {
		b, err := encodeResult(res)
		if err != nil {
			return nil, err
		}
		refs, summary = b.refs, b.summary
		r.ResultRefs, r.ResponseSummary = res.ResultRefs, b.summaryRaw
		r.ResponseCode, r.FailureCode = res.ResponseCode, res.FailureCode
		r.CompletedAt, r.ResponseExpiresAt = now, now.Add(ResponseTTL)
		code, completed, expires = res.ResponseCode, clock.Millis(now), clock.Millis(r.ResponseExpiresAt)
		if res.FailureCode != "" {
			failure = string(res.FailureCode)
		}
	}
	_, err := q.ExecContext(ctx, `INSERT INTO command_receipts (actor_id, project_scope, command_type, idempotency_key,
		operation_id, owner_module, request_hash, status, result_refs, response_code, response_summary, failure_code,
		created_at, completed_at, response_expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		cmd.ActorID, cmd.ProjectID, cmd.CommandType, cmd.IdempotencyKey, cmd.OperationID, s.module, cmd.RequestHash,
		status, refs, code, summary, failure, clock.Millis(now), completed, expires)
	if err != nil {
		if sqlite.IsUniqueViolation(err) {
			return nil, fmt.Errorf("%w: %v", ErrReceiptRace, err)
		}
		return nil, err
	}
	return r, nil
}

// ErrReceiptRace 表示并发写入了同一幂等键或 operation_id；调用方应重读回执后判定。
var ErrReceiptRace = errors.New("commands: receipt already exists")

type encodedResult struct {
	refs       string
	summary    any
	summaryRaw json.RawMessage
}

func encodeResult(res *Result) (encodedResult, error) {
	var out encodedResult
	for _, ref := range res.ResultRefs {
		if ref.Kind == "" || !ref.ID.Valid() {
			return out, fmt.Errorf("commands: invalid result ref %+v", ref)
		}
	}
	refs := res.ResultRefs
	if refs == nil {
		refs = []ResultRef{}
	}
	b, err := json.Marshal(refs)
	if err != nil {
		return out, err
	}
	out.refs = string(b)
	if res.Summary != nil {
		raw, err := json.Marshal(res.Summary)
		if err != nil {
			return out, fmt.Errorf("commands: marshal response summary: %w", err)
		}
		if len(raw) > MaxSummaryBytes {
			return out, fmt.Errorf("commands: response summary is %d bytes, limit %d", len(raw), MaxSummaryBytes)
		}
		out.summary, out.summaryRaw = string(raw), raw
	}
	return out, nil
}

// Result 是命令处理结果。
type Result struct {
	// Status 为 ReceiptSucceeded，或 ReceiptFailed 表示已接受的业务失败（占用幂等键）。
	Status       ReceiptStatus
	ResponseCode int
	// FailureCode 在 Status 为 failed 时必须是登记错误码。
	FailureCode errcode.Code
	Summary     any
	ResultRefs  []ResultRef
	Events      []event.Envelope
}

func (res *Result) validate() error {
	switch res.Status {
	case ReceiptSucceeded:
		if res.FailureCode != "" {
			return errors.New("commands: succeeded result must not carry a failure code")
		}
		if res.ResponseCode < 200 || res.ResponseCode > 299 {
			return fmt.Errorf("commands: succeeded result needs a 2xx response code, got %d", res.ResponseCode)
		}
	case ReceiptFailed:
		spec, ok := errcode.Lookup(res.FailureCode)
		if !ok {
			return fmt.Errorf("commands: failed result needs a registered failure code, got %q", res.FailureCode)
		}
		if res.ResponseCode == 0 {
			res.ResponseCode = spec.HTTPStatus
		}
		if res.ResponseCode != spec.HTTPStatus {
			return fmt.Errorf("commands: %s is registered with HTTP %d, result says %d", res.FailureCode, spec.HTTPStatus, res.ResponseCode)
		}
	default:
		return fmt.Errorf("commands: result status must be succeeded or failed, got %q", res.Status)
	}
	return nil
}

// Handler 在命令所属库的事务中执行业务变更。返回 error 时事务回滚且不写回执
// （无效请求不占键）；返回 Status=failed 的 Result 表示已接受的业务失败。
type Handler func(ctx context.Context, tx *sql.Tx) (Result, error)

// Response 是 Execute/Accept 的结果。
type Response struct {
	Outcome Outcome
	Receipt *Receipt
}

// Execute 在一个本库事务中完成：查回执 → 判定 → 业务变更 → outbox → 终态回执。
// 同键重复到达按 Decide 返回原结果；Replay 的结果在返回前必须由调用方按当前
// 读取权限复核。锁协调（Coordinator）由调用方在进入前取得。
func (s *Store) Execute(ctx context.Context, db *sql.DB, cmd Context, h Handler) (Response, error) {
	if err := s.checkCommand(cmd); err != nil {
		return Response{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Response{}, err
	}
	defer tx.Rollback()

	existing, err := s.LookupReceipt(ctx, tx, cmd.Key())
	if err != nil {
		return Response{}, err
	}
	now := s.clock.Now()
	if o := Decide(existing, cmd.RequestHash, now); o != OutcomeExecute {
		return Response{Outcome: o, Receipt: existing}, errOrNil(o.Err(existing))
	}
	res, err := h(ctx, tx)
	if err != nil {
		return Response{}, err
	}
	if err := res.validate(); err != nil {
		return Response{}, err
	}
	if err := s.appendOutbox(ctx, tx, cmd.OperationID, res.Events); err != nil {
		return Response{}, err
	}
	r, err := s.insertReceipt(ctx, tx, cmd, res.Status, &res, now)
	if err != nil {
		return Response{}, err
	}
	if err := tx.Commit(); err != nil {
		return Response{}, err
	}
	return Response{Outcome: OutcomeExecute, Receipt: r}, nil
}

func (s *Store) checkCommand(cmd Context) error {
	if err := cmd.Validate(); err != nil {
		return err
	}
	if !s.owns(cmd.CommandType) {
		return fmt.Errorf("%w: %s is not a %s command", ErrForeignCommand, cmd.CommandType, s.module)
	}
	return nil
}

// Operation 是持久操作记录。
type Operation struct {
	OperationID         ids.ID
	ParentOperationID   ids.ID
	OwnerModule         string
	CommandType         string
	RequestHash         digest.Digest
	TargetIDs           []string
	ExpectedRevisions   map[string]int64
	AuthRevision        int64
	RecoveryEpoch       int64
	Stage               Stage
	ManifestDigest      digest.Digest
	IntendedPaths       []string
	ImmutablePayloadRef string
	FailureCode         errcode.Code
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// Accept 为多阶段命令（例如文件提交）持久接受操作：在一个本库事务中执行
// prepare（例如保留版本号），写入 operations 与进行中回执。重复到达时
// 返回 InProgress（202，同一 operation_id）或原结果。
func (s *Store) Accept(ctx context.Context, db *sql.DB, cmd Context, stage Stage, targets []string, prepare func(ctx context.Context, tx *sql.Tx) error) (Response, error) {
	if err := s.checkCommand(cmd); err != nil {
		return Response{}, err
	}
	if stage != StageReceiving && stage != StagePrepared {
		return Response{}, fmt.Errorf("commands: operations must start in receiving or prepared, not %s", stage)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Response{}, err
	}
	defer tx.Rollback()

	existing, err := s.LookupReceipt(ctx, tx, cmd.Key())
	if err != nil {
		return Response{}, err
	}
	now := s.clock.Now()
	if o := Decide(existing, cmd.RequestHash, now); o != OutcomeExecute {
		return Response{Outcome: o, Receipt: existing}, errOrNil(o.Err(existing))
	}
	if prepare != nil {
		if err := prepare(ctx, tx); err != nil {
			return Response{}, err
		}
	}
	targetsJSON, _ := json.Marshal(nonNil(targets))
	exp := cmd.ExpectedRevisions
	if exp == nil {
		exp = map[string]int64{}
	}
	expJSON, _ := json.Marshal(exp)
	if _, err := tx.ExecContext(ctx, `INSERT INTO operations (operation_id, owner_module, command_type, request_hash,
		target_ids, expected_revisions, auth_revision, recovery_epoch, stage, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		cmd.OperationID, s.module, cmd.CommandType, cmd.RequestHash, string(targetsJSON), string(expJSON),
		cmd.PolicyRevision, cmd.RecoveryEpoch, stage, clock.Millis(now), clock.Millis(now)); err != nil {
		if sqlite.IsUniqueViolation(err) {
			return Response{}, fmt.Errorf("%w: %v", ErrReceiptRace, err)
		}
		return Response{}, err
	}
	r, err := s.insertReceipt(ctx, tx, cmd, ReceiptInProgress, nil, now)
	if err != nil {
		return Response{}, err
	}
	if err := tx.Commit(); err != nil {
		return Response{}, err
	}
	return Response{Outcome: OutcomeExecute, Receipt: r}, nil
}

func errOrNil(e *errcode.Error) error {
	if e == nil {
		return nil
	}
	return e
}

// Update 是阶段推进时可同时写入的字段；nil/零值表示不改，但推进到正常阶段
// receiving/prepared/installed/committed 时会清除当前 FailureCode。
type Update struct {
	ManifestDigest      digest.Digest
	IntendedPaths       []string
	ImmutablePayloadRef string
	FailureCode         errcode.Code
}

// Advance 在调用方事务中把本模块的操作从 from 推进到 to（不含终结，终结用 Complete）。
func (s *Store) Advance(ctx context.Context, q DBTX, opID ids.ID, from, to Stage, u Update) error {
	if to == StageCommitted || to == StageFailed || to == StageCancelled {
		return fmt.Errorf("commands: use Complete to finish an operation (%s)", to)
	}
	return s.advance(ctx, q, opID, from, to, u)
}

func (s *Store) advance(ctx context.Context, q DBTX, opID ids.ID, from, to Stage, u Update) error {
	if !CanTransition(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrStageConflict, from, to)
	}
	sets := []string{"stage = ?", "updated_at = ?"}
	args := []any{to, clock.Millis(s.clock.Now())}
	if u.ManifestDigest != "" {
		if !u.ManifestDigest.Valid() {
			return fmt.Errorf("commands: invalid manifest digest %q", u.ManifestDigest)
		}
		sets, args = append(sets, "manifest_digest = ?"), append(args, u.ManifestDigest)
	}
	if u.IntendedPaths != nil {
		b, _ := json.Marshal(u.IntendedPaths)
		sets, args = append(sets, "intended_paths = ?"), append(args, string(b))
	}
	if u.ImmutablePayloadRef != "" {
		sets, args = append(sets, "immutable_payload_ref = ?"), append(args, u.ImmutablePayloadRef)
	}
	if u.FailureCode != "" {
		if _, ok := errcode.Lookup(u.FailureCode); !ok {
			return fmt.Errorf("commands: unregistered failure code %q", u.FailureCode)
		}
		sets, args = append(sets, "failure_code = ?"), append(args, string(u.FailureCode))
	} else if to == StageReceiving || to == StagePrepared || to == StageInstalled || to == StageCommitted {
		// failure_code 描述当前阻塞/失败；恢复推进后不能继续污染正常状态视图。
		sets = append(sets, "failure_code = NULL")
	}
	args = append(args, opID, s.module, from)
	res, err := q.ExecContext(ctx, "UPDATE operations SET "+strings.Join(sets, ", ")+
		" WHERE operation_id = ? AND owner_module = ? AND stage = ?", args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("%w: %s expected in stage %s", ErrStageConflict, opID, from)
	}
	return nil
}

// Complete 在所属库的业务提交事务中终结操作：推进阶段到 committed/failed/cancelled，
// 写入 outbox，并把进行中回执改为终态。调用方的业务写入必须在同一 tx 中。
func (s *Store) Complete(ctx context.Context, tx *sql.Tx, opID ids.ID, from Stage, res Result) error {
	if err := res.validate(); err != nil {
		return err
	}
	to := StageCommitted
	if res.Status == ReceiptFailed {
		to = StageFailed
	}
	if err := s.advance(ctx, tx, opID, from, to, Update{FailureCode: res.FailureCode}); err != nil {
		return err
	}
	if err := s.appendOutbox(ctx, tx, opID, res.Events); err != nil {
		return err
	}
	b, err := encodeResult(&res)
	if err != nil {
		return err
	}
	now := s.clock.Now()
	var failure any
	if res.FailureCode != "" {
		failure = string(res.FailureCode)
	}
	out, err := tx.ExecContext(ctx, `UPDATE command_receipts SET status = ?, result_refs = ?, response_code = ?,
		response_summary = ?, failure_code = ?, completed_at = ?, response_expires_at = ?
		WHERE operation_id = ? AND owner_module = ? AND status = 'in_progress'`,
		res.Status, b.refs, res.ResponseCode, b.summary, failure, clock.Millis(now), clock.Millis(now.Add(ResponseTTL)),
		opID, s.module)
	if err != nil {
		return err
	}
	if n, _ := out.RowsAffected(); n != 1 {
		return fmt.Errorf("%w: no in-progress receipt for %s", ErrStageConflict, opID)
	}
	return nil
}

// Cancel 终结尚未提交的操作并记录失败回执：reason 必须是登记的错误码，
// 回执的 HTTP 状态取其登记值，之后同键重放同一结果。
func (s *Store) Cancel(ctx context.Context, tx *sql.Tx, opID ids.ID, from Stage, reason errcode.Code) error {
	spec, ok := errcode.Lookup(reason)
	if !ok {
		return fmt.Errorf("commands: cancel needs a registered reason code, got %q", reason)
	}
	responseCode := spec.HTTPStatus
	if !CanTransition(from, StageCancelled) {
		return fmt.Errorf("%w: %s -> cancelled", ErrStageConflict, from)
	}
	if err := s.advance(ctx, tx, opID, from, StageCancelled, Update{FailureCode: reason}); err != nil {
		return err
	}
	now := s.clock.Now()
	out, err := tx.ExecContext(ctx, `UPDATE command_receipts SET status = 'failed', failure_code = ?, response_code = ?,
		completed_at = ?, response_expires_at = ? WHERE operation_id = ? AND owner_module = ? AND status = 'in_progress'`,
		string(reason), responseCode, clock.Millis(now), clock.Millis(now.Add(ResponseTTL)), opID, s.module)
	if err != nil {
		return err
	}
	if n, _ := out.RowsAffected(); n != 1 {
		return fmt.Errorf("%w: no in-progress receipt for %s", ErrStageConflict, opID)
	}
	return nil
}

// GetOperation 读取操作记录。
func (s *Store) GetOperation(ctx context.Context, q DBTX, opID ids.ID) (*Operation, error) {
	var op Operation
	var parent, manifest, paths, payload, failure sql.NullString
	var targets, exp string
	var created, updated int64
	err := q.QueryRowContext(ctx, `SELECT operation_id, parent_operation_id, owner_module, command_type, request_hash,
		target_ids, expected_revisions, auth_revision, recovery_epoch, stage, manifest_digest, intended_paths,
		immutable_payload_ref, failure_code, created_at, updated_at FROM operations WHERE operation_id = ?`, opID).
		Scan(&op.OperationID, &parent, &op.OwnerModule, &op.CommandType, &op.RequestHash, &targets, &exp,
			&op.AuthRevision, &op.RecoveryEpoch, &op.Stage, &manifest, &paths, &payload, &failure, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	op.ParentOperationID = ids.ID(parent.String)
	op.ManifestDigest = digest.Digest(manifest.String)
	op.ImmutablePayloadRef = payload.String
	op.FailureCode = errcode.Code(failure.String)
	op.CreatedAt, op.UpdatedAt = clock.FromMillis(created), clock.FromMillis(updated)
	if err := json.Unmarshal([]byte(targets), &op.TargetIDs); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(exp), &op.ExpectedRevisions); err != nil {
		return nil, err
	}
	if paths.Valid {
		if err := json.Unmarshal([]byte(paths.String), &op.IntendedPaths); err != nil {
			return nil, err
		}
	}
	return &op, nil
}

// OpenOperations 列出本模块尚未终结的操作，供启动恢复分派（T08）。
func (s *Store) OpenOperations(ctx context.Context, q DBTX) ([]ids.ID, error) {
	rows, err := q.QueryContext(ctx, `SELECT operation_id FROM operations
		WHERE owner_module = ? AND stage NOT IN ('committed', 'failed', 'cancelled') ORDER BY created_at, operation_id`, s.module)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ids.ID
	for rows.Next() {
		var id ids.ID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// AppendEvents 在调用方的本库事务中写入 outbox，用于不经命令回执的状态变化
// （例如会话开始与结束）。事件类型必须登记为本模块所有，且都属于 opID。
func (s *Store) AppendEvents(ctx context.Context, q DBTX, opID ids.ID, events []event.Envelope) error {
	return s.appendOutbox(ctx, q, opID, events)
}

func (s *Store) appendOutbox(ctx context.Context, q DBTX, opID ids.ID, events []event.Envelope) error {
	// 所有写入口（Execute、Complete、AppendEvents）共用整批前检，拒绝越界
	// 事件时尚未写入任何 outbox 行；业务与回执仍由所属事务一同回滚。
	for _, e := range events {
		owner, err := ownership.EventOwner(e.EventType)
		if err != nil {
			return err
		}
		if owner != s.module {
			return fmt.Errorf("%w: event type %s belongs to %s", ErrForeignCommand, e.EventType, owner)
		}
		if e.OperationID != opID {
			return fmt.Errorf("commands: event %s belongs to operation %s, not %s", e.EventID, e.OperationID, opID)
		}
	}
	for _, e := range events {
		canonical, err := e.Canonical()
		if err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO outbox (event_id, operation_id, aggregate_id, aggregate_revision,
			event_type, schema_version, envelope, occurred_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			e.EventID, e.OperationID, e.AggregateID, e.AggregateRevision, e.EventType, e.SchemaVersion,
			string(canonical), clock.Millis(e.OccurredAt)); err != nil {
			return err
		}
	}
	return nil
}
