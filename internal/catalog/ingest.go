package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/catalog/pathrule"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/fileop"
)

// ProjectRequest 是登记项目的请求（只有系统管理员本人）。
type ProjectRequest struct {
	Who            authz.Context
	IdempotencyKey string
	Key            string
	Name           string
	ProjectType    string
	Summary        string
}

// Project 是项目登记与当前说明。
type Project struct {
	commit.Project
	Description ProjectDescription `json:"description"`
	// DescriptionPending 表示项目已登记但说明修订尚未生效（例如维护中），
	// 以同一幂等键重试即可补完。
	DescriptionPending bool         `json:"description_pending,omitempty"`
	DescriptionError   errcode.Code `json:"description_error,omitempty"`
}

// CreateProject 登记项目（稳定 ID 与不可变 key）并写入第一个项目说明修订。
// 同一幂等键重放返回原项目；key 已被占用 PATH_CONFLICT。
func (s *Service) CreateProject(ctx context.Context, req ProjectRequest) (Project, error) {
	if err := req.Who.Validate(); err != nil {
		return Project{}, err
	}
	if err := pathrule.CheckProjectKey(req.Key); err != nil {
		return Project{}, err
	}
	if req.Name == "" {
		req.Name = req.Key
	}
	if req.ProjectType == "" {
		req.ProjectType = "production"
	}
	name, summary := req.Name, req.Summary
	patch := ProjectPatch{Name: &name, ProjectType: &req.ProjectType, Summary: &summary}
	if _, err := patch.apply(ProjectDescription{}); err != nil {
		return Project{}, err
	}
	body, _ := json.Marshal(map[string]any{"key": req.Key, "name": req.Name, "project_type": req.ProjectType, "summary": req.Summary})
	hash, err := commands.RequestHash(commands.HashInput{CommandType: commit.CommandRegisterProject, Body: body})
	if err != nil {
		return Project{}, err
	}
	op, err := s.ids.New()
	if err != nil {
		return Project{}, err
	}
	cmd := commands.Context{OperationID: op, IdempotencyKey: req.IdempotencyKey, RequestHash: hash,
		CommandType: commit.CommandRegisterProject, ActorID: req.Who.PrincipalID, SessionID: req.Who.SessionID,
		RecoveryEpoch: req.Who.RecoveryEpoch}
	if err := cmd.Validate(); err != nil {
		return Project{}, invalid("invalid request: %v", err)
	}
	p, err := s.ledger.RegisterProject(ctx, cmd, commit.ProjectRequest{Who: req.Who, Key: req.Key})
	if err != nil {
		return Project{}, err
	}
	out := Project{Project: p}
	child, err := ids.DeriveChild(p.OperationID, "step:describe")
	if err != nil {
		return Project{}, err
	}
	target := commit.MetadataTarget{Kind: commit.TargetProject, ProjectID: p.ProjectID, ID: p.ProjectID}
	base := ProjectDescription{ProjectID: p.ProjectID, Key: p.Key}
	_, raw, err := s.commitRevision(ctx, revisionCommand{
		who: req.Who, key: "describe." + string(child), target: target, expected: 0, action: ActionPatchProject, opID: child,
		body: map[string]any{"kind": "project", "patch": patch},
		render: func(rev int64) ([]byte, error) {
			d, err := patch.apply(base)
			if err != nil {
				return nil, err
			}
			d.Revision, d.UpdatedBy = rev, req.Who.PrincipalID
			return renderProject(d)
		},
	})
	if err != nil {
		out.DescriptionPending, out.DescriptionError = true, errcode.CodeOf(err)
		out.Description = defaultProject(p)
		return out, nil
	}
	if err := parseInto(raw, ProjectContract, &out.Description); err != nil {
		return Project{}, err
	}
	return out, nil
}

func defaultProject(p commit.Project) ProjectDescription {
	return ProjectDescription{ProjectID: p.ProjectID, Key: p.Key, Name: p.Key, ProjectType: "production"}
}

// PatchProject 条件修改项目说明：expectedRevision 必须是当前生效修订（还没有
// 修订时为 0），否则 PRECONDITION_FAILED。
func (s *Service) PatchProject(ctx context.Context, who authz.Context, key string, projectID ids.ID, expectedRevision int64, patch ProjectPatch) (ProjectDescription, error) {
	if expectedRevision < 0 {
		return ProjectDescription{}, invalid("expected_revision must not be negative")
	}
	p, err := s.ledger.Project(ctx, projectID)
	if err != nil {
		return ProjectDescription{}, notFoundIfMissing(err)
	}
	if err := s.canRead(ctx, who, p.ProjectID, "project", p.ProjectID); err != nil {
		return ProjectDescription{}, err
	}
	if err := s.authorize(ctx, who, ActionPatchProject, authz.Resource{ProjectID: p.ProjectID, Kind: "project", ID: p.ProjectID}); err != nil {
		return ProjectDescription{}, err
	}
	target := commit.MetadataTarget{Kind: commit.TargetProject, ProjectID: p.ProjectID, ID: p.ProjectID}
	base, err := s.projectDescription(ctx, p)
	if err != nil {
		return ProjectDescription{}, err
	}
	_, raw, err := s.commitRevision(ctx, revisionCommand{
		who: who, key: key, target: target, expected: expectedRevision, action: ActionPatchProject,
		body: map[string]any{"kind": "project", "patch": patch},
		render: func(rev int64) ([]byte, error) {
			d, err := patch.apply(base)
			if err != nil {
				return nil, err
			}
			d.Revision, d.UpdatedBy = rev, who.PrincipalID
			return renderProject(d)
		},
	})
	if err != nil {
		return ProjectDescription{}, err
	}
	var d ProjectDescription
	return d, parseInto(raw, ProjectContract, &d)
}

// DeclaredUse 是提交时声明的输入：可读引用（可带代次）或永久引用。
type DeclaredUse struct {
	Ref             string `json:"ref,omitempty"`
	AliasGeneration int64  `json:"alias_generation,omitempty"`
	AssetID         ids.ID `json:"asset_id,omitempty"`
	VersionID       ids.ID `json:"version_id,omitempty"`
	Relation        string `json:"relation"`
}

// ContentInput 是待提交版本的内容。Rights 为空时沿用资产说明中的默认许可与
// 敏感级别（追加版本）；新建资产必须给出。
type ContentInput struct {
	Producer    *storage.Producer    `json:"producer,omitempty"`
	AssetType   manifest.AssetType   `json:"asset_type"`
	VersionNote string               `json:"version_note,omitempty"`
	Files       []manifest.InputFile `json:"files"`
	Uses        []DeclaredUse        `json:"uses,omitempty"`
	Rights      *manifest.Rights     `json:"rights,omitempty"`
	Metadata    map[string]any       `json:"metadata,omitempty"`
}

// VersionRequest 是一次入藏：新建资产的首版（AssetID 为空，给出 Slug）或
// 追加版本（给出 AssetID 与当前最新版本作为 BaseVersionID）。内容已经通过
// UploadID 对应的上传会话上传，或以来源授权复用；会话的操作即本次提交的操作。
type VersionRequest struct {
	Who            authz.Context
	IdempotencyKey string
	UploadID       ids.ID
	AssetID        ids.ID
	Slug           string
	BaseVersionID  ids.ID
	Content        ContentInput
	// Describe 只用于新建资产：第一个说明修订的标题、摘要、标签与主题。
	Describe *AssetPatch
}

// VersionResult 是入藏结果。
type VersionResult struct {
	OperationID     ids.ID           `json:"operation_id"`
	ProjectID       ids.ID           `json:"project_id"`
	AssetID         ids.ID           `json:"asset_id"`
	VersionID       ids.ID           `json:"version_id"`
	VersionNumber   int64            `json:"version_number"`
	AliasGeneration int64            `json:"alias_generation,omitempty"`
	Slug            string           `json:"slug"`
	ManifestDigest  digest.Digest    `json:"manifest_digest"`
	Ref             ids.PermanentRef `json:"ref"`
	URI             string           `json:"uri"`
	CommittedAt     time.Time        `json:"committed_at"`
	// Description 是新建资产的第一个说明修订；DescriptionPending 表示它尚未
	// 生效，以同一幂等键重试即可补完。
	Description        *AssetDescription `json:"description,omitempty"`
	DescriptionPending bool              `json:"description_pending,omitempty"`
	DescriptionError   errcode.Code      `json:"description_error,omitempty"`
}

// CommitVersion 执行一次入藏：规范化并冻结清单 → 台账保留（prepared，分配或
// 复用资产与版本身份、占名）→ storage 安装（installed，只接受本操作的复用
// 授权）→ 台账在最终接受边界复验后提交（committed，对外可见）→ 补写别名
// 历史、关闭上传会话、新建资产时写第一个说明修订。
//
// 同一幂等键重放返回同一版本，不会产生第二个版本；响应丢失后重试即可。安装
// 因内容与清单不符失败时放弃本次操作；缺少授权等可补救的失败保留 prepared，
// 补传后以同一幂等键重试。
func (s *Service) CommitVersion(ctx context.Context, req VersionRequest) (VersionResult, error) {
	if err := req.Who.Validate(); err != nil {
		return VersionResult{}, err
	}
	up, err := s.storage.GetUpload(ctx, req.Who, req.UploadID)
	if err != nil {
		return VersionResult{}, err
	}
	project, err := s.ledger.Project(ctx, up.ProjectID)
	if err != nil {
		return VersionResult{}, notFoundIfMissing(err)
	}
	if project.State != commit.ProjectActive {
		return VersionResult{}, reasonErr(errcode.InvalidStateTransition, "project_not_active", "the project does not accept new versions")
	}
	create := req.AssetID == ""
	action := commit.ActionCommitVersion
	if create {
		action = ActionCreateAsset
		if req.BaseVersionID != "" {
			return VersionResult{}, errcode.New(errcode.BaseVersionConflict, "a new asset has no base version")
		}
		if req.Slug, err = pathrule.NormalizeSlug(req.Slug); err != nil {
			return VersionResult{}, err
		}
		if req.Describe != nil {
			if _, err := req.Describe.apply(AssetDescription{}); err != nil {
				return VersionResult{}, err
			}
		}
	} else if req.Slug != "" || req.Describe != nil {
		return VersionResult{}, invalid("slug and describe only apply when creating an asset")
	}
	if err := s.authorize(ctx, req.Who, action, authz.Resource{ProjectID: project.ProjectID, Kind: "project", ID: project.ProjectID}); err != nil {
		return VersionResult{}, err
	}
	var asset commit.Asset
	if !create {
		if asset, err = s.ledger.Asset(ctx, req.AssetID); err != nil || asset.ProjectID != project.ProjectID {
			return VersionResult{}, errcode.New(errcode.NotFound, "")
		}
	}
	hash, err := versionRequestHash(req, project.ProjectID)
	if err != nil {
		return VersionResult{}, err
	}
	cmd := commands.Context{OperationID: up.OperationID, IdempotencyKey: req.IdempotencyKey, RequestHash: hash,
		CommandType: commit.CommandType, ActorID: req.Who.PrincipalID, SessionID: req.Who.SessionID,
		ProjectID: project.ProjectID, RecoveryEpoch: req.Who.RecoveryEpoch}
	if err := cmd.Validate(); err != nil {
		return VersionResult{}, invalid("invalid request: %v", err)
	}
	// 先按声明请求查询原保留。重试不再解析 @latest 或读取现在的默认许可，
	// 已提交后的响应重放也不依赖已经清理的 staging 文件。
	p, err := s.ledger.LookupPrepared(ctx, cmd)
	var content manifest.Content
	var md digest.Digest
	switch {
	case err == nil:
		view, err := s.ledger.Operation(ctx, p.OperationID)
		if err != nil {
			return VersionResult{}, err
		}
		if view.Stage == commands.StageCommitted || view.Stage == commands.StageProjected {
			c, err := s.ledger.Version(ctx, p.AssetID, p.VersionID)
			if err != nil {
				return VersionResult{}, err
			}
			return s.afterCommit(ctx, req, c)
		}
		if view.Stage.Final() || view.Stage == commands.StageQuarantined {
			return VersionResult{}, errcode.New(errcode.InvalidStateTransition, "the operation can no longer commit")
		}
		if content, err = s.thaw(p.OperationID, p.ManifestDigest); err != nil {
			// 并发重放可能在上述读状态之后完成提交并清理 staging。
			if view, readErr := s.ledger.Operation(ctx, p.OperationID); readErr == nil &&
				(view.Stage == commands.StageCommitted || view.Stage == commands.StageProjected) {
				c, readErr := s.ledger.Version(ctx, p.AssetID, p.VersionID)
				if readErr != nil {
					return VersionResult{}, readErr
				}
				return s.afterCommit(ctx, req, c)
			}
			return VersionResult{}, err
		}
		md = p.ManifestDigest
	case errcode.CodeOf(err) == errcode.NotFound:
		in, err := s.freezeInput(ctx, req, asset, create)
		if err != nil {
			return VersionResult{}, err
		}
		if content, err = manifest.Normalize(in); err != nil {
			return VersionResult{}, err
		}
		if !create {
			if err := s.checkAssetType(ctx, req.AssetID, content.AssetType); err != nil {
				return VersionResult{}, err
			}
		}
		if md, err = content.Digest(); err != nil {
			return VersionResult{}, err
		}
		if err := s.freeze(ctx, up.OperationID, md, content); err != nil {
			return VersionResult{}, err
		}
		p, err = s.ledger.Prepare(ctx, cmd, commit.PrepareRequest{Who: req.Who, ProjectID: project.ProjectID, AssetID: req.AssetID,
			Slug: req.Slug, BaseVersionID: req.BaseVersionID, ManifestDigest: md, Files: content.InstallFiles()})
		if err != nil {
			return VersionResult{}, err
		}
	default:
		return VersionResult{}, err
	}
	c, err := s.installAndCommit(ctx, req.Who, p, md, content)
	if err != nil {
		return VersionResult{}, err
	}
	return s.afterCommit(ctx, req, c)
}

// versionRequestHash 只覆盖声明形式。默认许可、浮动引用的解析结果不进入
// 请求摘要；这些事实在首次受理时冻结，重试时按台账保留恢复。
func versionRequestHash(req VersionRequest, project ids.ID) (digest.Digest, error) {
	declared := slices.Clone(req.Content.Uses)
	if declared == nil {
		declared = []DeclaredUse{}
	}
	body := map[string]any{"upload_id": req.UploadID, "asset_id": req.AssetID, "slug": req.Slug, "base_version_id": req.BaseVersionID,
		"asset_type": req.Content.AssetType, "version_note": req.Content.VersionNote, "files": req.Content.Files, "uses": declared,
		"rights": req.Content.Rights, "metadata": req.Content.Metadata, "describe": req.Describe}
	if err := canonjson.CheckUTF8(body); err != nil {
		return "", invalid("the request must contain valid UTF-8")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", invalid("the request must contain valid JSON values: %v", err)
	}
	targets := []string{}
	if req.AssetID != "" {
		targets = append(targets, string(req.AssetID))
	}
	return commands.RequestHash(commands.HashInput{CommandType: commit.CommandType, ProjectID: project, Targets: targets, Body: raw})
}

// freezeInput 解析声明的 uses 为固定引用，并补齐首次冻结时的默认许可。
func (s *Service) freezeInput(ctx context.Context, req VersionRequest, asset commit.Asset, create bool) (manifest.Input, error) {
	c := req.Content
	rightsIn := c.Rights
	if rightsIn == nil {
		if create {
			return manifest.Input{}, errcode.New(errcode.SchemaInvalid, "rights are required when creating an asset").
				WithDetails(errcode.PointerDetail("rights_required", "/content/rights", "declare usage, license and sensitivity"))
		}
		r, err := s.defaultRights(ctx, asset)
		if err != nil {
			return manifest.Input{}, err
		}
		rightsIn = &r
	}
	uses := make([]manifest.Use, 0, len(c.Uses))
	for i, u := range c.Uses {
		var res Resolved
		var err error
		switch {
		case u.Ref != "" && (u.AssetID != "" || u.VersionID != ""):
			return manifest.Input{}, invalid("uses[%d] must choose a ref or a permanent reference, not both", i)
		case u.Ref != "":
			res, err = s.Resolve(ctx, req.Who, u.Ref, u.AliasGeneration)
		case u.AssetID != "" || u.VersionID != "":
			if u.AliasGeneration != 0 {
				return manifest.Input{}, invalid("uses[%d].alias_generation does not apply to a permanent reference", i)
			}
			res, err = s.ResolvePermanent(ctx, req.Who, ids.PermanentRef{AssetID: u.AssetID, VersionID: u.VersionID})
		default:
			err = invalid("uses[%d] needs a ref or a permanent reference", i)
		}
		if err != nil {
			return manifest.Input{}, err
		}
		purpose := usePurpose(u.Relation, rightsIn.Usage)
		d, err := s.rights.EvaluateUse(ctx, req.Who, res.Ref, purpose)
		if err != nil {
			return manifest.Input{}, err
		}
		if err := d.Err(); err != nil {
			return manifest.Input{}, err
		}
		uses = append(uses, manifest.Use{InstanceID: s.instance, AssetID: res.AssetID, VersionID: res.VersionID, Relation: u.Relation, Declared: u.Ref})
	}
	return manifest.Input{AssetType: c.AssetType, BaseVersionID: req.BaseVersionID, VersionNote: c.VersionNote,
		Files: c.Files, Uses: uses, Rights: *rightsIn, Metadata: c.Metadata, Producer: c.Producer}, nil
}

func usePurpose(relation, usage string) authz.Purpose {
	if relation == "reference" || usage != "production" {
		return authz.PurposeReference
	}
	return authz.PurposeProduction
}

// defaultRights 取资产说明中的默认许可与敏感级别，缺项时取最新版本的声明。
func (s *Service) defaultRights(ctx context.Context, a commit.Asset) (manifest.Rights, error) {
	latest, err := s.ledger.LatestVersion(ctx, a.AssetID)
	if err != nil {
		return manifest.Rights{}, err
	}
	doc, err := s.readManifest(ctx, latest)
	if err != nil {
		return manifest.Rights{}, err
	}
	r := doc.Content.Rights
	d, err := s.assetDescription(ctx, a)
	if err != nil {
		return manifest.Rights{}, err
	}
	if d.Defaults != nil {
		if d.Defaults.Usage != "" {
			r.Usage = d.Defaults.Usage
		}
		if d.Defaults.License != "" {
			r.License = d.Defaults.License
		}
	}
	if d.Sensitivity != "" {
		r.Sensitivity = d.Sensitivity
	}
	return r, nil
}

// checkAssetType 核对追加版本的类型与资产一致：资产类型建立后不变。
func (s *Service) checkAssetType(ctx context.Context, asset ids.ID, t manifest.AssetType) error {
	latest, err := s.ledger.LatestVersion(ctx, asset)
	if err != nil {
		return err
	}
	doc, err := s.readManifest(ctx, latest)
	if err != nil {
		return err
	}
	if doc.Content.AssetType != t {
		return errcode.New(errcode.SchemaInvalid, "the asset type cannot change").
			WithDetails(errcode.PointerDetail("asset_type_immutable", "/content/asset_type", "the asset is a "+string(doc.Content.AssetType)))
	}
	return nil
}

// frozenDir 是一次操作的冻结内容暂存：按操作与清单摘要寻址，崩溃后重试时用它
// 还原 prepared 时冻结的内容（浮动引用可能已经解析到别的版本）。操作提交后删除。
func (s *Service) frozenDir(op ids.ID) string {
	return filepath.Join(s.home, "staging", "frozen", string(op))
}

func (s *Service) frozenPath(op ids.ID, md digest.Digest) string {
	return filepath.Join(s.frozenDir(op), md.Hex()+".json")
}

func (s *Service) freeze(ctx context.Context, op ids.ID, md digest.Digest, c manifest.Content) error {
	raw, err := canonjson.CanonicalizeValue(c)
	if err != nil {
		return err
	}
	if digest.Of(raw) != md {
		return errcode.New(errcode.Internal, "")
	}
	_, release, err := s.write(ctx)
	if err != nil {
		return err
	}
	defer release()
	_, err = s.fs.CreateOrMatch(s.frozenPath(op, md), raw, 0o444)
	if errors.Is(err, fileop.ErrContentDiffers) {
		return errcode.New(errcode.OperationNeedsReconciliation, "frozen content for this digest differs")
	}
	return err
}

func (s *Service) thaw(op ids.ID, md digest.Digest) (manifest.Content, error) {
	raw, err := os.ReadFile(s.frozenPath(op, md))
	if errors.Is(err, fs.ErrNotExist) {
		return manifest.Content{}, errcode.New(errcode.OperationNeedsReconciliation, "the frozen content of the prepared operation is missing")
	}
	if err != nil {
		return manifest.Content{}, fileop.Wrap("reading frozen content", err)
	}
	if digest.Of(raw) != md {
		return manifest.Content{}, errcode.New(errcode.HashMismatch, "the frozen content no longer matches its digest")
	}
	var c manifest.Content
	dec := json.NewDecoder(bytesReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&c); err != nil {
		return manifest.Content{}, err
	}
	return c, nil
}

// installAndCommit 安装并提交 prepared 操作；已提交的操作直接返回原结果。
func (s *Service) installAndCommit(ctx context.Context, who authz.Context, p commit.Prepared, md digest.Digest, content manifest.Content) (commit.Committed, error) {
	if view, err := s.ledger.Operation(ctx, p.OperationID); err == nil &&
		(view.Stage == commands.StageCommitted || view.Stage == commands.StageProjected) {
		return s.ledger.Version(ctx, p.AssetID, p.VersionID)
	}
	if p.ManifestDigest != md {
		// 重放 prepared 时内容已冻结：还原当时的内容，不采用这次重新解析的结果。
		c, err := s.thaw(p.OperationID, p.ManifestDigest)
		if err != nil {
			return commit.Committed{}, err
		}
		content = c
	}
	doc := manifest.Document{InstanceID: s.instance, ProjectID: p.ProjectID, AssetID: p.AssetID, VersionID: p.VersionID,
		VersionNumber: p.VersionNumber, OperationID: p.OperationID, CreatedBy: p.ActorID, SessionID: p.SessionID,
		ManifestDigest: p.ManifestDigest, Content: content}
	raw, err := doc.Render()
	if err != nil {
		return commit.Committed{}, err
	}
	ireq := p.InstallRequest()
	ireq.Manifest = raw
	proof, err := s.storage.Install(ctx, ireq)
	if err != nil {
		switch errcode.CodeOf(err) {
		case errcode.HashMismatch, errcode.SchemaInvalid, errcode.PathConflict:
			// 清单与已上传内容不符：同一请求重试也不会成功，放弃操作以释放保留。
			_ = s.ledger.Cancel(ctx, p.OperationID, who)
		}
		return commit.Committed{}, err
	}
	return s.ledger.Commit(ctx, p.OperationID, who, proof)
}

// afterCommit 补完提交后的文件侧工作；它们各自幂等，失败不改变已提交的结果。
func (s *Service) afterCommit(ctx context.Context, req VersionRequest, c commit.Committed) (VersionResult, error) {
	a, err := s.ledger.Asset(ctx, c.AssetID)
	if err != nil {
		return VersionResult{}, err
	}
	ref := c.Ref(s.instance)
	out := VersionResult{OperationID: c.OperationID, ProjectID: c.ProjectID, AssetID: c.AssetID, VersionID: c.VersionID,
		VersionNumber: c.VersionNumber, AliasGeneration: c.AliasGeneration, Slug: a.Slug, ManifestDigest: c.ManifestDigest,
		Ref: ref, URI: ids.URIScheme + "://" + string(ref.InstanceID) + "/assets/" + string(ref.AssetID) + "/versions/" + string(ref.VersionID),
		CommittedAt: c.CommittedAt}
	_ = s.storage.CompleteUpload(ctx, req.Who, req.UploadID)
	if _, release, err := s.write(ctx); err == nil {
		_ = os.RemoveAll(s.frozenDir(c.OperationID))
		release()
	}
	if c.AliasGeneration == 0 {
		return out, nil
	}
	if err := s.writeAliasRecord(ctx, a); err != nil {
		// 历史文件可由 RepairAliasHistory 按台账补齐；当前代次的解析不依赖它。
		_ = err
	}
	d, err := s.describeNew(ctx, req, a, c)
	if err != nil {
		out.DescriptionPending, out.DescriptionError = true, errcode.CodeOf(err)
		return out, nil
	}
	out.Description = &d
	return out, nil
}

// describeNew 为新建资产写入第一个说明修订（由提交操作派生的子操作，重放幂等）。
func (s *Service) describeNew(ctx context.Context, req VersionRequest, a commit.Asset, c commit.Committed) (AssetDescription, error) {
	child, err := ids.DeriveChild(c.OperationID, "step:describe")
	if err != nil {
		return AssetDescription{}, err
	}
	base, err := s.derivedDescription(ctx, a)
	if err != nil {
		return AssetDescription{}, err
	}
	patch := AssetPatch{}
	if req.Describe != nil {
		patch = *req.Describe
	}
	target := commit.MetadataTarget{Kind: commit.TargetAsset, ProjectID: a.ProjectID, ID: a.AssetID}
	_, raw, err := s.commitRevision(ctx, revisionCommand{
		who: req.Who, key: "describe." + string(child), target: target, expected: 0, action: ActionPatchOwnMetadata, opID: child,
		body: map[string]any{"kind": "asset", "patch": patch},
		render: func(rev int64) ([]byte, error) {
			d, err := patch.apply(base)
			if err != nil {
				return nil, err
			}
			d.Revision, d.UpdatedBy = rev, req.Who.PrincipalID
			return renderAsset(d)
		},
	})
	if err != nil {
		return AssetDescription{}, err
	}
	var d AssetDescription
	return d, parseInto(raw, AssetContract, &d)
}

// PatchAsset 条件修改资产说明：expectedRevision 必须是当前生效修订（还没有
// 修订时为 0），否则 PRECONDITION_FAILED。制作者修改自己建立的资产按
// catalog.patch_own_metadata 授权，其余按 catalog.patch_metadata；敏感级别与
// 默认许可只能经专门命令修改。
func (s *Service) PatchAsset(ctx context.Context, who authz.Context, key string, assetID ids.ID, expectedRevision int64, patch AssetPatch) (AssetDescription, error) {
	if expectedRevision < 0 {
		return AssetDescription{}, invalid("expected_revision must not be negative")
	}
	a, err := s.ledger.Asset(ctx, assetID)
	if err != nil {
		return AssetDescription{}, notFoundIfMissing(err)
	}
	if err := s.canRead(ctx, who, a.ProjectID, "asset", a.AssetID); err != nil {
		return AssetDescription{}, err
	}
	action := ActionPatchMetadata
	if a.CreatedBy == who.PrincipalID {
		action = ActionPatchOwnMetadata
	}
	res := authz.Resource{ProjectID: a.ProjectID, Kind: "asset", ID: a.AssetID}
	if err := s.authorize(ctx, who, action, res); err != nil {
		if action == ActionPatchMetadata {
			return AssetDescription{}, err
		}
		// 本人建立的资产：负责人或整理者也可按普通著录权限修改。
		if err2 := s.authorize(ctx, who, ActionPatchMetadata, res); err2 != nil {
			return AssetDescription{}, err
		}
		action = ActionPatchMetadata
	}
	if patch.Sensitivity != nil || patch.Defaults != nil {
		return AssetDescription{}, errcode.New(errcode.FieldRequiresSpecialCommand,
			"sensitivity and default rights can only be changed with a dedicated rights command")
	}
	base, err := s.assetDescription(ctx, a)
	if err != nil {
		return AssetDescription{}, err
	}
	target := commit.MetadataTarget{Kind: commit.TargetAsset, ProjectID: a.ProjectID, ID: a.AssetID}
	_, raw, err := s.commitRevision(ctx, revisionCommand{
		who: who, key: key, target: target, expected: expectedRevision, action: action,
		body: map[string]any{"kind": "asset", "patch": patch},
		render: func(rev int64) ([]byte, error) {
			d, err := patch.apply(base)
			if err != nil {
				return nil, err
			}
			d.Revision, d.UpdatedBy = rev, who.PrincipalID
			return renderAsset(d)
		},
	})
	if err != nil {
		return AssetDescription{}, err
	}
	var d AssetDescription
	return d, parseInto(raw, AssetContract, &d)
}

// CancelVersion 由本人放弃一次尚未提交的入藏：台账释放保留（新资产的占名随之
// 释放、不消耗代次），上传会话关闭，未消费的复用授权撤销。
func (s *Service) CancelVersion(ctx context.Context, who authz.Context, uploadID ids.ID) error {
	up, err := s.storage.GetUpload(ctx, who, uploadID)
	if err != nil {
		return err
	}
	if view, err := s.ledger.Operation(ctx, up.OperationID); err == nil && !view.Stage.Final() && view.Stage != commands.StageProjected {
		if err := s.ledger.Cancel(ctx, up.OperationID, who); err != nil && errcode.CodeOf(err) != errcode.InvalidStateTransition {
			return err
		}
	}
	return s.storage.CancelUpload(ctx, who, uploadID)
}

// readManifest 读取并解析已提交版本的清单文件，核对其身份与台账一致。
func (s *Service) readManifest(ctx context.Context, v commit.Committed) (manifest.Document, error) {
	raw, err := s.storage.ReadManifest(ctx, v)
	if err != nil {
		return manifest.Document{}, err
	}
	doc, err := manifest.Parse(raw)
	if err != nil {
		return manifest.Document{}, err
	}
	if doc.AssetID != v.AssetID || doc.VersionID != v.VersionID || doc.VersionNumber != v.VersionNumber ||
		doc.ManifestDigest != v.ManifestDigest || doc.ProjectID != v.ProjectID || doc.InstanceID != s.instance ||
		doc.OperationID != v.OperationID || doc.CreatedBy != v.CommittedBy {
		return manifest.Document{}, errcode.New(errcode.OperationNeedsReconciliation, "the manifest file does not match the ledger")
	}
	return doc, nil
}
