// Package committest 提供 commit.Ledger 与 commit.Reader 的内存桩，以及可对
// 任意实现运行的契约测试套件（contract.go）。storage（T02）等模块可以用
// 本桩开发，不必等待台账整卡完成；台账的真实实现须通过同一套件。
package committest

import (
	"context"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/install"
)

// Authority 是台账桩需要的授权事实：当前授权判定与实例恢复代次。
type Authority interface {
	authz.Authorizer
	authz.EpochSource
}

// Memory 是内存台账桩。
type Memory struct {
	mu        sync.Mutex
	clock     clock.Clock
	authz     Authority
	installer install.Installer

	keys     map[commands.ReceiptKey]ids.ID // 幂等键 → operation
	ops      map[ids.ID]*op
	claims   map[string]ids.ID // project/slug → asset
	assets   map[ids.ID]*asset
	versions map[ids.ID]commit.Committed // 已提交版本
}

type op struct {
	cmd       commands.Context
	who       authz.Context
	prepared  commit.Prepared
	base      ids.ID
	stage     commands.Stage
	failure   errcode.Code
	committed *commit.Committed
	created   time.Time
	updated   time.Time
}

type asset struct {
	project    ids.ID
	slug       string
	latest     ids.ID // 最新已提交版本
	nextNumber int64
	pending    ids.ID // 进行中的 operation
}

var (
	_ commit.Ledger = (*Memory)(nil)
	_ commit.Reader = (*Memory)(nil)
)

// New 创建内存台账；authority 用于 Prepare 与最终接受边界的授权复验及恢复
// 代次核对，installer 用于提交前复核安装证明。
func New(clk clock.Clock, authorizer Authority, installer install.Installer) *Memory {
	return &Memory{
		clock: clk, authz: authorizer, installer: installer,
		keys: map[commands.ReceiptKey]ids.ID{}, ops: map[ids.ID]*op{}, claims: map[string]ids.ID{},
		assets: map[ids.ID]*asset{}, versions: map[ids.ID]commit.Committed{},
	}
}

// Prepare 实现 commit.Ledger。
func (m *Memory) Prepare(ctx context.Context, cmd commands.Context, req commit.PrepareRequest) (commit.Prepared, error) {
	if err := cmd.Validate(); err != nil {
		return commit.Prepared{}, err
	}
	if cmd.CommandType != commit.CommandType || cmd.ProjectID != req.ProjectID || cmd.ActorID != req.Who.PrincipalID {
		return commit.Prepared{}, errcode.New(errcode.SchemaInvalid, "command context does not match the prepare request")
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.keys[cmd.Key()]; ok {
		o := m.ops[existing]
		if o.cmd.RequestHash != cmd.RequestHash {
			return commit.Prepared{}, errcode.New(errcode.IdempotencyConflict, "").WithOperation(string(existing))
		}
		return o.prepared, nil
	}
	if err := m.authorize(ctx, req.Who, req.ProjectID); err != nil {
		return commit.Prepared{}, err
	}
	for i, f := range req.Files {
		if i > 0 && req.Files[i-1].Path >= f.Path {
			return commit.Prepared{}, errcode.New(errcode.SchemaInvalid, "files must be sorted by path and unique")
		}
	}
	if !req.ManifestDigest.Valid() {
		return commit.Prepared{}, errcode.New(errcode.SchemaInvalid, "manifest_digest")
	}

	var a *asset
	assetID := req.AssetID
	claimKey := ""
	if assetID == "" {
		if req.Slug == "" {
			return commit.Prepared{}, errcode.New(errcode.SchemaInvalid, "slug is required for a new asset")
		}
		claimKey = string(req.ProjectID) + "/" + req.Slug
		if _, taken := m.claims[claimKey]; taken {
			return commit.Prepared{}, errcode.New(errcode.PathConflict, "")
		}
		if req.BaseVersionID != "" {
			return commit.Prepared{}, errcode.New(errcode.BaseVersionConflict, "a new asset has no base version")
		}
		assetID = ids.New()
		a = &asset{project: req.ProjectID, slug: req.Slug, nextNumber: 1}
	} else {
		a = m.assets[assetID]
		if a == nil || a.project != req.ProjectID {
			return commit.Prepared{}, errcode.New(errcode.NotFound, "")
		}
		if a.pending != "" {
			return commit.Prepared{}, errcode.New(errcode.ResourceBusy, "").WithOperation(string(a.pending))
		}
		if req.BaseVersionID != a.latest {
			return commit.Prepared{}, errcode.New(errcode.BaseVersionConflict, "")
		}
	}

	now := m.clock.Now()
	p := commit.Prepared{
		OperationID: cmd.OperationID, ProjectID: req.ProjectID, AssetID: assetID, VersionID: ids.New(),
		VersionNumber: a.nextNumber, ManifestDigest: req.ManifestDigest, Files: slices.Clone(req.Files),
	}
	a.nextNumber++ // 保留的版本号即使取消也不回收
	a.pending = cmd.OperationID
	if claimKey != "" {
		m.claims[claimKey] = assetID // 占名随 prepared 保留，与资产身份绑定
	}
	m.assets[assetID] = a
	m.keys[cmd.Key()] = cmd.OperationID
	m.ops[cmd.OperationID] = &op{cmd: cmd, who: req.Who, prepared: p, base: req.BaseVersionID,
		stage: commands.StagePrepared, created: now, updated: now}
	return p, nil
}

func (m *Memory) authorize(ctx context.Context, who authz.Context, project ids.ID) error {
	d, err := m.authz.Authorize(ctx, who, commit.ActionCommitVersion, authz.Resource{ProjectID: project, Kind: "project", ID: project})
	if err != nil {
		return err
	}
	return d.Err()
}

// Commit 实现 commit.Ledger。
func (m *Memory) Commit(ctx context.Context, opID ids.ID, who authz.Context, proof install.Proof) (commit.Committed, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o := m.ops[opID]
	if o == nil {
		return commit.Committed{}, errcode.New(errcode.NotFound, "")
	}
	proofDigest, err := proof.Digest()
	if err != nil {
		return commit.Committed{}, errcode.Wrap(errcode.SchemaInvalid, "invalid install proof", err)
	}
	switch o.stage {
	case commands.StageCommitted:
		if o.committed.ProofDigest != proofDigest {
			return commit.Committed{}, errcode.New(errcode.OperationNeedsReconciliation, "operation already committed with a different install proof").WithOperation(string(opID))
		}
		return *o.committed, nil
	case commands.StageQuarantined, commands.StageCancelled, commands.StageFailed:
		return commit.Committed{}, errcode.New(errcode.InvalidStateTransition, "operation can no longer commit").WithOperation(string(opID))
	}
	if who.PrincipalID != o.cmd.ActorID {
		return commit.Committed{}, errcode.New(errcode.Forbidden, "only the accepting actor can complete this operation")
	}
	// 最终接受边界：读取当前授权，不使用 Prepare 时的判定。
	if err := m.authorize(ctx, who, o.prepared.ProjectID); err != nil {
		o.stage, o.failure, o.updated = commands.StageBlocked, errcode.CodeOf(err), m.clock.Now()
		return commit.Committed{}, err
	}
	// 整馆恢复前接受、尚未提交的操作不能直接继续，先对账。
	current, err := m.authz.RecoveryEpoch(ctx)
	if err != nil {
		return commit.Committed{}, err
	}
	if err := execution.CheckRecoveryEpoch(execution.SubjectOperation, o.cmd.RecoveryEpoch, current); err != nil {
		o.stage, o.failure, o.updated = commands.StageBlocked, errcode.OperationNeedsReconciliation, m.clock.Now()
		return commit.Committed{}, err
	}
	if err := proof.Matches(o.prepared.InstallRequest()); err != nil {
		m.quarantine(ctx, o, errcode.OperationNeedsReconciliation)
		return commit.Committed{}, err
	}
	if err := m.installer.Verify(ctx, proof); err != nil {
		m.quarantine(ctx, o, errcode.CodeOf(err))
		return commit.Committed{}, err
	}
	a := m.assets[o.prepared.AssetID]
	if a.latest != o.base {
		o.stage, o.failure = commands.StageFailed, errcode.BaseVersionConflict
		a.release(opID)
		return commit.Committed{}, errcode.New(errcode.BaseVersionConflict, "")
	}
	c := commit.Committed{
		OperationID: opID, ProjectID: o.prepared.ProjectID, AssetID: o.prepared.AssetID,
		VersionID: o.prepared.VersionID, VersionNumber: o.prepared.VersionNumber,
		ManifestDigest: o.prepared.ManifestDigest, ProofDigest: proofDigest, CommittedAt: m.clock.Now(),
	}
	m.versions[c.VersionID] = c
	a.latest = c.VersionID
	a.release(opID)
	o.stage, o.committed, o.updated = commands.StageCommitted, &c, c.CommittedAt
	return c, nil
}

func (m *Memory) quarantine(ctx context.Context, o *op, reason errcode.Code) {
	_ = m.installer.Quarantine(ctx, o.prepared.OperationID, reason) // 隔离失败不影响“不提交”的结论
	o.stage, o.failure, o.updated = commands.StageQuarantined, reason, m.clock.Now()
	m.assets[o.prepared.AssetID].release(o.prepared.OperationID)
}

// release 只释放属于 opID 的占用；已被其他操作取得的占用保持不变。
func (a *asset) release(opID ids.ID) {
	if a.pending == opID {
		a.pending = ""
	}
}

// Cancel 放弃尚未提交的操作；已保留的版本号不回收，占名随新资产释放。
func (m *Memory) Cancel(opID ids.ID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	o := m.ops[opID]
	if o == nil {
		return errcode.New(errcode.NotFound, "")
	}
	if o.stage.Final() {
		return errcode.New(errcode.InvalidStateTransition, "")
	}
	o.stage, o.failure, o.updated = commands.StageCancelled, errcode.InvalidStateTransition, m.clock.Now()
	a := m.assets[o.prepared.AssetID]
	a.release(opID)
	// 新资产既无已提交版本、也没有其他进行中的操作时才释放占名。
	if a.latest == "" && a.pending == "" {
		delete(m.claims, string(a.project)+"/"+a.slug)
	}
	return nil
}

// Operation 实现 commit.Ledger。
func (m *Memory) Operation(_ context.Context, opID ids.ID) (*commands.View, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o := m.ops[opID]
	if o == nil {
		return nil, errcode.New(errcode.NotFound, "")
	}
	v := &commands.View{
		OperationID: opID, OwnerModule: "ledger", CommandType: commit.CommandType, Stage: o.stage,
		CreatedAt: o.created, UpdatedAt: o.updated,
	}
	switch o.stage {
	case commands.StagePrepared, commands.StageInstalled:
		v.Retryable, v.NextAction = true, errcode.ActionPollOperation
	case commands.StageBlocked:
		v.NextAction = errcode.ActionHumanAction
	case commands.StageQuarantined:
		v.NextAction = errcode.ActionReconcile
	default:
		v.NextAction = errcode.ActionNone
	}
	if o.committed != nil {
		v.ResultRefs = []commands.ResultRef{{Kind: "version", ID: o.committed.VersionID, Revision: 1}}
	}
	if spec, ok := errcode.Lookup(o.failure); ok {
		v.Reason = &commands.Reason{Code: o.failure, Message: spec.Summary}
	}
	return v, nil
}

// Version 实现 commit.Reader。
func (m *Memory) Version(_ context.Context, assetID, versionID ids.ID) (commit.Committed, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.versions[versionID]
	if !ok {
		return commit.Committed{}, errcode.New(errcode.NotFound, "")
	}
	if c.AssetID != assetID {
		return commit.Committed{}, errcode.New(errcode.RefMismatch, "")
	}
	return c, nil
}

// Versions 实现 commit.Reader。
func (m *Memory) Versions(_ context.Context, after ids.ID, limit int) ([]commit.Committed, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []commit.Committed
	for _, c := range m.versions {
		if c.VersionID > after {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VersionID < out[j].VersionID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ManifestDigest 是测试辅助：对文件列表计算一个稳定的清单摘要。
func ManifestDigest(files []install.File) digest.Digest {
	h := digest.NewHasher()
	for _, f := range files {
		h.Write([]byte(f.Path + "\x00" + f.SHA256 + "\x00"))
	}
	return h.Digest()
}
