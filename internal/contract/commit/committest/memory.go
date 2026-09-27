// Package committest 提供 commit 接口（Ledger、Reader、Namespace、Metadata、
// Projects）的内存桩，以及可对任意实现运行的契约测试套件（contract.go）。
// storage 与 catalog（T02）等模块可以用本桩开发，不必等待台账整卡完成；台账
// 的真实实现须通过同一套件。
package committest

import (
	"context"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog/pathrule"
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
	mu         sync.Mutex
	clock      clock.Clock
	authz      Authority
	installer  install.Installer
	revisions  commit.RevisionVerifier
	gate       *commands.Gate
	acceptance commit.AcceptanceVerifier

	keys     map[commands.ReceiptKey]ids.ID // 幂等键 → operation
	ops      map[ids.ID]*op
	claims   map[string]*claim // project + "\x00" + pathrule.Key(slug)
	assets   map[ids.ID]*asset
	versions map[ids.ID]commit.Committed // 已提交版本

	metaOps  map[ids.ID]*metaOp
	current  map[commit.MetadataTarget]commit.CommittedMetadata
	metaBusy map[commit.MetadataTarget]ids.ID

	projects    map[ids.ID]commit.Project
	projectKeys map[string]ids.ID
	projectOps  map[ids.ID]projectOp
}

type projectOp struct {
	project commit.Project
	hash    digest.Digest
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

type claim struct {
	c commit.Claim
	// prev 是本次保留之前的占名；未提交就取消时恢复，保留不消耗代次。
	prev *commit.Claim
}

type asset struct {
	project    ids.ID
	slug       string
	generation int64
	latest     ids.ID // 最新已提交版本
	nextNumber int64
	pending    ids.ID // 进行中的 operation
	byNumber   map[int64]ids.ID
	committed  *commit.Asset
}

type metaOp struct {
	cmd       commands.Context
	req       commit.MetadataRequest
	prepared  commit.PreparedMetadata
	stage     commands.Stage
	failure   errcode.Code
	committed *commit.CommittedMetadata
}

var (
	_ commit.Ledger    = (*Memory)(nil)
	_ commit.Reader    = (*Memory)(nil)
	_ commit.Namespace = (*Memory)(nil)
	_ commit.Metadata  = (*Memory)(nil)
	_ commit.Projects  = (*Memory)(nil)
)

// New 创建内存台账；authority 用于授权复验及恢复代次核对，installer 用于
// 提交前复核安装证明。说明修订的文件复核用 SetRevisionVerifier 另行接入。
func New(clk clock.Clock, authorizer Authority, installer install.Installer) *Memory {
	return &Memory{
		clock: clk, authz: authorizer, installer: installer,
		keys: map[commands.ReceiptKey]ids.ID{}, ops: map[ids.ID]*op{}, claims: map[string]*claim{},
		assets: map[ids.ID]*asset{}, versions: map[ids.ID]commit.Committed{},
		metaOps: map[ids.ID]*metaOp{}, current: map[commit.MetadataTarget]commit.CommittedMetadata{},
		metaBusy: map[commit.MetadataTarget]ids.ID{},
		projects: map[ids.ID]commit.Project{}, projectKeys: map[string]ids.ID{}, projectOps: map[ids.ID]projectOp{},
	}
}

// SetInstaller 接入安装器（用于真实 storage 与台账桩互相引用时的组装）。
func (m *Memory) SetInstaller(in install.Installer) {
	m.mu.Lock()
	m.installer = in
	m.mu.Unlock()
}

// SetRevisionVerifier 接入说明修订文件的复核（catalog）。未设置时 CommitMetadata 拒绝。
func (m *Memory) SetRevisionVerifier(v commit.RevisionVerifier) {
	m.mu.Lock()
	m.revisions = v
	m.mu.Unlock()
}

// SetGate 接入与身份模块相同的维护屏障及 security_guard。
func (m *Memory) SetGate(g *commands.Gate) {
	m.mu.Lock()
	m.gate = g
	m.mu.Unlock()
}

// SetAcceptanceVerifier 接入目录与存储的最终输入复验。生产台账必须提供同样的边界。
func (m *Memory) SetAcceptanceVerifier(v commit.AcceptanceVerifier) {
	m.mu.Lock()
	m.acceptance = v
	m.mu.Unlock()
}

func (m *Memory) acceptanceLock(ctx context.Context) (context.Context, func(), error) {
	m.mu.Lock()
	g := m.gate
	m.mu.Unlock()
	if g == nil {
		return ctx, func() {}, nil
	}
	lctx, held, err := g.Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return ctx, nil, err
	}
	return lctx, held.Release, nil
}

// LookupPrepared 只按已认证调用方的回执作用域读取原保留，不重新解析浮动输入。
func (m *Memory) LookupPrepared(_ context.Context, cmd commands.Context) (commit.Prepared, error) {
	if err := cmd.Validate(); err != nil {
		return commit.Prepared{}, err
	}
	if cmd.CommandType != commit.CommandType {
		return commit.Prepared{}, errcode.New(errcode.SchemaInvalid, "unexpected command type")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.keys[cmd.Key()]
	if !ok {
		return commit.Prepared{}, errcode.New(errcode.NotFound, "")
	}
	o := m.ops[id]
	if o == nil || o.cmd.RequestHash != cmd.RequestHash {
		return commit.Prepared{}, errcode.New(errcode.IdempotencyConflict, "").WithOperation(string(id))
	}
	p := o.prepared
	p.Files = slices.Clone(p.Files)
	return p, nil
}

func claimKey(project ids.ID, slug string) string {
	return string(project) + "\x00" + pathrule.Key(slug)
}

// Prepare 实现 commit.Ledger。
func (m *Memory) Prepare(ctx context.Context, cmd commands.Context, req commit.PrepareRequest) (commit.Prepared, error) {
	lctx, release, err := m.acceptanceLock(ctx)
	if err != nil {
		return commit.Prepared{}, err
	}
	defer release()
	ctx = lctx
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
		if o == nil || o.cmd.RequestHash != cmd.RequestHash {
			return commit.Prepared{}, errcode.New(errcode.IdempotencyConflict, "").WithOperation(string(existing))
		}
		return o.prepared, nil
	}
	if _, used := m.ops[cmd.OperationID]; used {
		return commit.Prepared{}, errcode.New(errcode.IdempotencyConflict, "operation id already used by another request").WithOperation(string(cmd.OperationID))
	}
	if err := m.authorize(ctx, req.Who, commit.ActionCommitVersion, req.ProjectID, "project", req.ProjectID); err != nil {
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
	var key string
	var generation int64
	if assetID == "" {
		if err := pathrule.CheckSlug(req.Slug); err != nil {
			return commit.Prepared{}, err
		}
		if req.BaseVersionID != "" {
			return commit.Prepared{}, errcode.New(errcode.BaseVersionConflict, "a new asset has no base version")
		}
		key = claimKey(req.ProjectID, req.Slug)
		if c := m.claims[key]; c != nil && c.c.State != commit.ClaimReleased {
			return commit.Prepared{}, errcode.New(errcode.PathConflict, "")
		}
		generation = 1
		if c := m.claims[key]; c != nil {
			generation = c.c.Generation + 1
		}
		assetID = ids.New()
		a = &asset{project: req.ProjectID, slug: req.Slug, generation: generation, nextNumber: 1, byNumber: map[int64]ids.ID{}}
	} else {
		a = m.assets[assetID]
		if a == nil || a.project != req.ProjectID || a.committed == nil {
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
		OperationID: cmd.OperationID, ActorID: cmd.ActorID, SessionID: cmd.SessionID, ProjectID: req.ProjectID, AssetID: assetID, VersionID: ids.New(),
		VersionNumber: a.nextNumber, ManifestDigest: req.ManifestDigest, Files: slices.Clone(req.Files),
		AliasGeneration: generation,
	}
	a.nextNumber++ // 保留的版本号即使取消也不回收
	a.pending = cmd.OperationID
	if key != "" {
		// 占名随 prepared 保留，与资产身份绑定；提交时生效。
		c := &claim{c: commit.Claim{ProjectID: req.ProjectID, Slug: req.Slug, State: commit.ClaimReserved,
			Generation: generation, AssetID: assetID, OperationID: cmd.OperationID, Revision: 1}}
		if prev := m.claims[key]; prev != nil {
			saved := prev.c
			c.prev = &saved
			c.c.Revision = prev.c.Revision + 1
		}
		m.claims[key] = c
	}
	m.assets[assetID] = a
	m.keys[cmd.Key()] = cmd.OperationID
	m.ops[cmd.OperationID] = &op{cmd: cmd, who: req.Who, prepared: p, base: req.BaseVersionID,
		stage: commands.StagePrepared, created: now, updated: now}
	return p, nil
}

func (m *Memory) authorize(ctx context.Context, who authz.Context, action authz.Action, project ids.ID, kind string, id ids.ID) error {
	d, err := m.authz.Authorize(ctx, who, action, authz.Resource{ProjectID: project, Kind: kind, ID: id})
	if err != nil {
		return err
	}
	return d.Err()
}

// finalCheck 是最终接受边界的授权与恢复代次复验。
func (m *Memory) finalCheck(ctx context.Context, cmd commands.Context, who authz.Context, action authz.Action, project ids.ID, kind string, id ids.ID) (errcode.Code, error) {
	if err := m.authorize(ctx, who, action, project, kind, id); err != nil {
		return errcode.CodeOf(err), err
	}
	// 整馆恢复前接受、尚未提交的操作不能直接继续，先对账。
	current, err := m.authz.RecoveryEpoch(ctx)
	if err != nil {
		return "", err
	}
	if err := execution.CheckRecoveryEpoch(execution.SubjectOperation, cmd.RecoveryEpoch, current); err != nil {
		return errcode.OperationNeedsReconciliation, err
	}
	return "", nil
}

// Commit 实现 commit.Ledger。
func (m *Memory) Commit(ctx context.Context, opID ids.ID, who authz.Context, proof install.Proof) (commit.Committed, error) {
	m.mu.Lock()
	o := m.ops[opID]
	if o == nil {
		m.mu.Unlock()
		return commit.Committed{}, errcode.New(errcode.NotFound, "")
	}
	p, final, verifier := o.prepared, o.stage.Final(), m.acceptance
	p.Files = slices.Clone(p.Files)
	m.mu.Unlock()
	// 已完成的重放不重新执行验收；未完成的接受在统一写屏障内串联当前输入复验和提交。
	if final {
		return m.commit(ctx, opID, who, proof)
	}
	lctx, release, err := m.acceptanceLock(ctx)
	if err != nil {
		return commit.Committed{}, err
	}
	if verifier != nil {
		// 不持有桩的 mutex：复验会通过 Reader 读取其他已提交的输入版本。
		if err := verifier.VerifyAcceptance(lctx, who, p); err != nil {
			m.mu.Lock()
			if !o.stage.Final() {
				o.stage, o.failure, o.updated = commands.StageBlocked, errcode.CodeOf(err), m.clock.Now()
			}
			committed := o.stage == commands.StageCommitted
			m.mu.Unlock()
			release()
			if committed {
				return m.commit(ctx, opID, who, proof)
			}
			return commit.Committed{}, err
		}
	}
	result, err := m.commit(lctx, opID, who, proof)
	m.mu.Lock()
	quarantined, reason, installer := o.stage == commands.StageQuarantined, o.failure, m.installer
	m.mu.Unlock()
	release()
	// 物理隔离自行进入 Gate，不能在持有 security_guard 时重入屏障。
	if quarantined && installer != nil {
		_ = installer.Quarantine(ctx, opID, reason)
	}
	return result, err
}

func (m *Memory) commit(ctx context.Context, opID ids.ID, who authz.Context, proof install.Proof) (commit.Committed, error) {
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
	if code, err := m.finalCheck(ctx, o.cmd, who, commit.ActionCommitVersion, o.prepared.ProjectID, "project", o.prepared.ProjectID); err != nil {
		if code != "" {
			o.stage, o.failure, o.updated = commands.StageBlocked, code, m.clock.Now()
		}
		return commit.Committed{}, err
	}
	if err := proof.Matches(o.prepared.InstallRequest()); err != nil {
		m.quarantine(o, errcode.OperationNeedsReconciliation)
		return commit.Committed{}, err
	}
	if err := m.installer.Verify(ctx, proof); err != nil {
		m.quarantine(o, errcode.CodeOf(err))
		return commit.Committed{}, err
	}
	a := m.assets[o.prepared.AssetID]
	if a.latest != o.base {
		o.stage, o.failure = commands.StageFailed, errcode.BaseVersionConflict
		m.release(o)
		return commit.Committed{}, errcode.New(errcode.BaseVersionConflict, "")
	}
	c := commit.Committed{
		OperationID: opID, ProjectID: o.prepared.ProjectID, AssetID: o.prepared.AssetID,
		VersionID: o.prepared.VersionID, VersionNumber: o.prepared.VersionNumber,
		ManifestDigest: o.prepared.ManifestDigest, ProofDigest: proofDigest, CommittedAt: m.clock.Now(),
		CommittedBy: o.cmd.ActorID, AliasGeneration: o.prepared.AliasGeneration,
	}
	m.versions[c.VersionID] = c
	a.latest = c.VersionID
	a.byNumber[c.VersionNumber] = c.VersionID
	a.pending = ""
	if a.committed == nil {
		a.committed = &commit.Asset{AssetID: c.AssetID, ProjectID: c.ProjectID, Slug: a.slug, Generation: a.generation,
			CreatedBy: c.CommittedBy, CreatedAt: c.CommittedAt, OperationID: opID}
		if cl := m.claims[claimKey(a.project, a.slug)]; cl != nil && cl.c.OperationID == opID {
			cl.c.State, cl.prev = commit.ClaimActive, nil
			cl.c.Revision++
		}
	}
	o.stage, o.committed, o.updated, o.failure = commands.StageCommitted, &c, c.CommittedAt, ""
	return c, nil
}

func (m *Memory) quarantine(o *op, reason errcode.Code) {
	o.stage, o.failure, o.updated = commands.StageQuarantined, reason, m.clock.Now()
	m.release(o)
}

// release 只释放属于 o 的占用：资产的进行中操作，以及新建资产尚未生效的占名
// （恢复到保留之前的状态，不消耗代次）。已被其他操作取得的占用保持不变。
func (m *Memory) release(o *op) {
	a := m.assets[o.prepared.AssetID]
	if a == nil {
		return
	}
	if a.pending == o.prepared.OperationID {
		a.pending = ""
	}
	if a.committed != nil {
		return
	}
	key := claimKey(a.project, a.slug)
	if cl := m.claims[key]; cl != nil && cl.c.OperationID == o.prepared.OperationID && cl.c.State == commit.ClaimReserved {
		if cl.prev != nil {
			restored := *cl.prev
			restored.Revision = cl.c.Revision + 1
			m.claims[key] = &claim{c: restored}
		} else {
			delete(m.claims, key)
		}
	}
}

// Cancel 实现 commit.Ledger：只有操作的发起主体可以放弃。
func (m *Memory) Cancel(ctx context.Context, opID ids.ID, who authz.Context) error {
	_, release, err := m.acceptanceLock(ctx)
	if err != nil {
		return err
	}
	defer release()
	m.mu.Lock()
	o := m.ops[opID]
	if o == nil || o.cmd.ActorID != who.PrincipalID {
		m.mu.Unlock()
		return errcode.New(errcode.NotFound, "")
	}
	m.mu.Unlock()
	return m.CancelOperation(opID)
}

// CancelOperation 是恢复流程使用的放弃（不核对发起者）：已保留的版本号不回收，
// 新资产的占名释放。
func (m *Memory) CancelOperation(opID ids.ID) error {
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
	m.release(o)
	return nil
}

// ReleaseName 是测试构造器：模拟 M2 的宽限期整资产删除或管理员释放名称，
// 把当前 active 占名改为 released。代次与原资产保留，之后同名新资产取得
// 下一代次。M1 没有对应的业务命令。
func (m *Memory) ReleaseName(project ids.ID, slug string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cl := m.claims[claimKey(project, slug)]
	if cl == nil || cl.c.State != commit.ClaimActive {
		return errcode.New(errcode.InvalidStateTransition, "only an active claim can be released")
	}
	cl.c.State = commit.ClaimReleased
	cl.c.Revision++
	return nil
}

// Operation 实现 commit.Ledger。
func (m *Memory) Operation(_ context.Context, opID ids.ID) (*commands.View, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var (
		stage      commands.Stage
		failure    errcode.Code
		cmdType    string
		created    time.Time
		updated    time.Time
		resultRefs []commands.ResultRef
	)
	switch {
	case m.ops[opID] != nil:
		o := m.ops[opID]
		stage, failure, cmdType, created, updated = o.stage, o.failure, commit.CommandType, o.created, o.updated
		if o.committed != nil {
			resultRefs = []commands.ResultRef{{Kind: "version", ID: o.committed.VersionID, Revision: 1}}
		}
	case m.metaOps[opID] != nil:
		o := m.metaOps[opID]
		stage, failure, cmdType = o.stage, o.failure, commit.CommandCommitMetadata
		created, updated = m.clock.Now(), m.clock.Now()
		if o.committed != nil {
			resultRefs = []commands.ResultRef{{Kind: string(o.committed.Target.Kind), ID: o.committed.Target.ID, Revision: o.committed.Revision}}
			created, updated = o.committed.CommittedAt, o.committed.CommittedAt
		}
	default:
		return nil, errcode.New(errcode.NotFound, "")
	}
	v := &commands.View{
		OperationID: opID, OwnerModule: "ledger", CommandType: cmdType, Stage: stage,
		CreatedAt: created, UpdatedAt: updated, ResultRefs: resultRefs,
	}
	switch stage {
	case commands.StagePrepared, commands.StageInstalled:
		v.Retryable, v.NextAction = true, errcode.ActionPollOperation
	case commands.StageBlocked:
		v.NextAction = errcode.ActionHumanAction
	case commands.StageQuarantined:
		v.NextAction = errcode.ActionReconcile
	default:
		v.NextAction = errcode.ActionNone
	}
	if spec, ok := errcode.Lookup(failure); ok {
		v.Reason = &commands.Reason{Code: failure, Message: spec.Summary}
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

// VersionByNumber 实现 commit.Reader。
func (m *Memory) VersionByNumber(_ context.Context, assetID ids.ID, number int64) (commit.Committed, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.assets[assetID]
	if a == nil || a.committed == nil {
		return commit.Committed{}, errcode.New(errcode.NotFound, "")
	}
	id, ok := a.byNumber[number]
	if !ok {
		return commit.Committed{}, errcode.New(errcode.NotFound, "")
	}
	return m.versions[id], nil
}

// LatestVersion 实现 commit.Reader。
func (m *Memory) LatestVersion(_ context.Context, assetID ids.ID) (commit.Committed, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.assets[assetID]
	if a == nil || a.latest == "" {
		return commit.Committed{}, errcode.New(errcode.NotFound, "")
	}
	return m.versions[a.latest], nil
}

// Asset 实现 commit.Reader。
func (m *Memory) Asset(_ context.Context, assetID ids.ID) (commit.Asset, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.assets[assetID]
	if a == nil || a.committed == nil {
		return commit.Asset{}, errcode.New(errcode.NotFound, "")
	}
	return *a.committed, nil
}

// Claim 实现 commit.Namespace。
func (m *Memory) Claim(_ context.Context, project ids.ID, slug string) (commit.Claim, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cl := m.claims[claimKey(project, slug)]
	if cl == nil {
		return commit.Claim{}, errcode.New(errcode.NotFound, "")
	}
	return cl.c, nil
}

// PrepareMetadata 实现 commit.Metadata。
func (m *Memory) PrepareMetadata(ctx context.Context, cmd commands.Context, req commit.MetadataRequest) (commit.PreparedMetadata, error) {
	lctx, release, err := m.acceptanceLock(ctx)
	if err != nil {
		return commit.PreparedMetadata{}, err
	}
	defer release()
	ctx = lctx
	if err := cmd.Validate(); err != nil {
		return commit.PreparedMetadata{}, err
	}
	if cmd.CommandType != commit.CommandCommitMetadata || cmd.ProjectID != req.Target.ProjectID || cmd.ActorID != req.Who.PrincipalID {
		return commit.PreparedMetadata{}, errcode.New(errcode.SchemaInvalid, "command context does not match the metadata request")
	}
	if !req.ContentDigest.Valid() || !req.Action.Valid() || req.ExpectedRevision < 0 {
		return commit.PreparedMetadata{}, errcode.New(errcode.SchemaInvalid, "metadata request is incomplete")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.keys[cmd.Key()]; ok {
		o := m.metaOps[existing]
		if o == nil || o.cmd.RequestHash != cmd.RequestHash {
			return commit.PreparedMetadata{}, errcode.New(errcode.IdempotencyConflict, "").WithOperation(string(existing))
		}
		return o.prepared, nil
	}
	if _, used := m.metaOps[cmd.OperationID]; used {
		return commit.PreparedMetadata{}, errcode.New(errcode.IdempotencyConflict, "operation id already used by another request").WithOperation(string(cmd.OperationID))
	}
	if err := m.authorize(ctx, req.Who, req.Action, req.Target.ProjectID, string(req.Target.Kind), req.Target.ID); err != nil {
		return commit.PreparedMetadata{}, err
	}
	if !m.targetExists(req.Target) {
		return commit.PreparedMetadata{}, errcode.New(errcode.NotFound, "")
	}
	if pending, busy := m.metaBusy[req.Target]; busy {
		return commit.PreparedMetadata{}, errcode.New(errcode.ResourceBusy, "").WithOperation(string(pending))
	}
	if cur := m.current[req.Target].Revision; cur != req.ExpectedRevision {
		return commit.PreparedMetadata{}, errcode.Newf(errcode.PreconditionFailed, "current revision is %d", cur)
	}
	p := commit.PreparedMetadata{OperationID: cmd.OperationID, Target: req.Target, Revision: req.ExpectedRevision + 1, ContentDigest: req.ContentDigest}
	m.keys[cmd.Key()] = cmd.OperationID
	m.metaOps[cmd.OperationID] = &metaOp{cmd: cmd, req: req, prepared: p, stage: commands.StagePrepared}
	m.metaBusy[req.Target] = cmd.OperationID
	return p, nil
}

func (m *Memory) targetExists(t commit.MetadataTarget) bool {
	switch t.Kind {
	case commit.TargetProject:
		_, ok := m.projects[t.ID]
		return ok && t.ID == t.ProjectID
	case commit.TargetAsset:
		a := m.assets[t.ID]
		return a != nil && a.committed != nil && a.project == t.ProjectID
	}
	return false
}

// CommitMetadata 实现 commit.Metadata。
func (m *Memory) CommitMetadata(ctx context.Context, opID ids.ID, who authz.Context, proof commit.RevisionProof) (commit.CommittedMetadata, error) {
	lctx, release, err := m.acceptanceLock(ctx)
	if err != nil {
		return commit.CommittedMetadata{}, err
	}
	defer release()
	ctx = lctx
	m.mu.Lock()
	defer m.mu.Unlock()
	o := m.metaOps[opID]
	if o == nil {
		return commit.CommittedMetadata{}, errcode.New(errcode.NotFound, "")
	}
	switch o.stage {
	case commands.StageCommitted:
		return *o.committed, nil
	case commands.StageQuarantined, commands.StageCancelled, commands.StageFailed:
		return commit.CommittedMetadata{}, errcode.New(errcode.InvalidStateTransition, "operation can no longer commit").WithOperation(string(opID))
	}
	if who.PrincipalID != o.cmd.ActorID {
		return commit.CommittedMetadata{}, errcode.New(errcode.Forbidden, "only the accepting actor can complete this operation")
	}
	t := o.req.Target
	if code, err := m.finalCheck(ctx, o.cmd, who, o.req.Action, t.ProjectID, string(t.Kind), t.ID); err != nil {
		if code != "" {
			o.stage, o.failure = commands.StageBlocked, code
		}
		return commit.CommittedMetadata{}, err
	}
	p := o.prepared
	if proof.OperationID != opID || proof.Target != p.Target || proof.Revision != p.Revision || proof.ContentDigest != p.ContentDigest {
		o.stage, o.failure = commands.StageQuarantined, errcode.OperationNeedsReconciliation
		delete(m.metaBusy, t)
		return commit.CommittedMetadata{}, errcode.New(errcode.OperationNeedsReconciliation, "revision proof does not match the prepared revision")
	}
	if m.revisions == nil {
		return commit.CommittedMetadata{}, errcode.New(errcode.OperationNeedsReconciliation, "no revision verifier configured")
	}
	if err := m.revisions.VerifyRevision(ctx, proof); err != nil {
		o.stage, o.failure = commands.StageQuarantined, errcode.CodeOf(err)
		delete(m.metaBusy, t)
		return commit.CommittedMetadata{}, err
	}
	c := commit.CommittedMetadata{OperationID: opID, Target: t, Revision: p.Revision, ContentDigest: p.ContentDigest,
		CommittedAt: m.clock.Now(), CommittedBy: o.cmd.ActorID}
	m.current[t] = c
	delete(m.metaBusy, t)
	o.stage, o.committed = commands.StageCommitted, &c
	return c, nil
}

// CancelMetadata 放弃尚未生效的修订；修订号不回收。
func (m *Memory) CancelMetadata(opID ids.ID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	o := m.metaOps[opID]
	if o == nil {
		return errcode.New(errcode.NotFound, "")
	}
	if o.stage.Final() {
		return errcode.New(errcode.InvalidStateTransition, "")
	}
	o.stage, o.failure = commands.StageCancelled, errcode.InvalidStateTransition
	if m.metaBusy[o.req.Target] == opID {
		delete(m.metaBusy, o.req.Target)
	}
	return nil
}

// CurrentMetadata 实现 commit.Metadata。
func (m *Memory) CurrentMetadata(_ context.Context, t commit.MetadataTarget) (commit.CommittedMetadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.current[t]
	if !ok {
		return commit.CommittedMetadata{}, errcode.New(errcode.NotFound, "")
	}
	return c, nil
}

// RegisterProject 实现 commit.Projects。
func (m *Memory) RegisterProject(ctx context.Context, cmd commands.Context, req commit.ProjectRequest) (commit.Project, error) {
	lctx, release, err := m.acceptanceLock(ctx)
	if err != nil {
		return commit.Project{}, err
	}
	defer release()
	ctx = lctx
	if err := cmd.Validate(); err != nil {
		return commit.Project{}, err
	}
	if cmd.CommandType != commit.CommandRegisterProject || cmd.ProjectID != "" || cmd.ActorID != req.Who.PrincipalID {
		return commit.Project{}, errcode.New(errcode.SchemaInvalid, "command context does not match the project request")
	}
	if err := pathrule.CheckProjectKey(req.Key); err != nil {
		return commit.Project{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.keys[cmd.Key()]; ok {
		po, found := m.projectOps[existing]
		if !found || po.hash != cmd.RequestHash {
			return commit.Project{}, errcode.New(errcode.IdempotencyConflict, "").WithOperation(string(existing))
		}
		// 重放按当前权限复核后返回原结果。
		if err := m.authorize(ctx, req.Who, commit.ActionCreateProject, "", "project", ""); err != nil {
			return commit.Project{}, err
		}
		return po.project, nil
	}
	if _, err := m.finalCheck(ctx, cmd, req.Who, commit.ActionCreateProject, "", "project", ""); err != nil {
		return commit.Project{}, err
	}
	if _, taken := m.projectKeys[req.Key]; taken {
		return commit.Project{}, errcode.New(errcode.PathConflict, "project key is already registered")
	}
	p := commit.Project{ProjectID: ids.New(), Key: req.Key, State: commit.ProjectActive, CreatedAt: m.clock.Now(),
		CreatedBy: req.Who.PrincipalID, OperationID: cmd.OperationID}
	m.projects[p.ProjectID] = p
	m.projectKeys[p.Key] = p.ProjectID
	m.projectOps[cmd.OperationID] = projectOp{project: p, hash: cmd.RequestHash}
	m.keys[cmd.Key()] = cmd.OperationID
	return p, nil
}

// Project 实现 commit.Projects。
func (m *Memory) Project(_ context.Context, id ids.ID) (commit.Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.projects[id]
	if !ok {
		return commit.Project{}, errcode.New(errcode.NotFound, "")
	}
	return p, nil
}

// ProjectByKey 实现 commit.Projects。
func (m *Memory) ProjectByKey(_ context.Context, key string) (commit.Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.projectKeys[key]
	if !ok {
		return commit.Project{}, errcode.New(errcode.NotFound, "")
	}
	return m.projects[id], nil
}

// ManifestDigest 是测试辅助：对文件列表计算一个稳定的清单摘要。
func ManifestDigest(files []install.File) digest.Digest {
	h := digest.NewHasher()
	for _, f := range files {
		h.Write([]byte(f.Path + "\x00" + f.SHA256 + "\x00"))
	}
	return h.Digest()
}
