package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/storage/fileop"
)

// 说明文件的契约标识。
const (
	AssetContract   = "lantai.asset/v1"
	ProjectContract = "lantai.project/v1"
)

// Defaults 是新版本的默认许可声明。
type Defaults struct {
	Usage   string `json:"usage,omitempty"`
	License string `json:"license,omitempty"`
}

// AssetDescription 是资产说明的一个修订。Revision 为 0 表示还没有已提交的
// 说明修订，内容由登记事实推导。
type AssetDescription struct {
	AssetID     ids.ID             `json:"asset_id"`
	ProjectID   ids.ID             `json:"project_id"`
	AssetType   manifest.AssetType `json:"asset_type"`
	Slug        string             `json:"slug"`
	Title       string             `json:"title"`
	Summary     string             `json:"summary,omitempty"`
	Tags        []string           `json:"tags"`
	Subjects    []string           `json:"subjects"`
	Sensitivity string             `json:"sensitivity"`
	Defaults    *Defaults          `json:"defaults,omitempty"`
	Revision    int64              `json:"revision"`
	UpdatedBy   ids.ID             `json:"updated_by"`
	Extra       map[string]any     `json:"extra,omitempty"`
}

// ProjectDescription 是项目说明的一个修订。
type ProjectDescription struct {
	ProjectID   ids.ID         `json:"project_id"`
	Key         string         `json:"key"`
	Name        string         `json:"name"`
	ProjectType string         `json:"project_type"`
	Summary     string         `json:"summary,omitempty"`
	Revision    int64          `json:"revision"`
	UpdatedBy   ids.ID         `json:"updated_by"`
	Extra       map[string]any `json:"extra,omitempty"`
}

// ProjectTypes 是项目类型。
var ProjectTypes = []string{"production", "library", "reference", "sandbox"}

// AssetPatch 是著录修改；nil 字段表示不改。敏感级别与默认许可是安全相关
// 字段，普通著录接口一律拒绝（FIELD_REQUIRES_SPECIAL_COMMAND）。
type AssetPatch struct {
	Title    *string         `json:"title,omitempty"`
	Summary  *string         `json:"summary,omitempty"`
	Tags     *[]string       `json:"tags,omitempty"`
	Subjects *[]string       `json:"subjects,omitempty"`
	Extra    *map[string]any `json:"extra,omitempty"`

	Sensitivity *string   `json:"sensitivity,omitempty"`
	Defaults    *Defaults `json:"defaults,omitempty"`
}

// ProjectPatch 是项目说明修改；nil 字段表示不改。
type ProjectPatch struct {
	Name        *string         `json:"name,omitempty"`
	ProjectType *string         `json:"project_type,omitempty"`
	Summary     *string         `json:"summary,omitempty"`
	Extra       *map[string]any `json:"extra,omitempty"`
}

func cleanText(field, s string, max int) (string, error) {
	if !utf8.ValidString(s) {
		return "", invalid("%s is not valid UTF-8", field)
	}
	s = strings.TrimSpace(norm.NFC.String(s))
	if utf8.RuneCountInString(s) > max {
		return "", invalid("%s is longer than %d characters", field, max)
	}
	return s, nil
}

// cleanSet 规范化标签类集合：NFC、去首尾空白、去重、排序（集合语义，重排不改变内容）。
func cleanSet(field string, in []string) ([]string, error) {
	out := []string{}
	for _, v := range in {
		c, err := cleanText(field, v, 64)
		if err != nil {
			return nil, err
		}
		if c == "" {
			return nil, invalid("%s must not contain empty values", field)
		}
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	if len(out) > 64 {
		return nil, invalid("%s has more than 64 values", field)
	}
	sort.Strings(out)
	return out, nil
}

// apply 把修改应用到说明上；安全相关字段拒绝。
func (p AssetPatch) apply(d AssetDescription) (AssetDescription, error) {
	if p.Sensitivity != nil || p.Defaults != nil {
		return d, errcode.New(errcode.FieldRequiresSpecialCommand,
			"sensitivity and default rights can only be changed with a dedicated rights command")
	}
	var err error
	if p.Title != nil {
		if d.Title, err = cleanText("title", *p.Title, 200); err != nil {
			return d, err
		}
		if d.Title == "" {
			return d, invalid("title must not be empty")
		}
	}
	if p.Summary != nil {
		if d.Summary, err = cleanText("summary", *p.Summary, 4000); err != nil {
			return d, err
		}
	}
	if p.Tags != nil {
		if d.Tags, err = cleanSet("tags", *p.Tags); err != nil {
			return d, err
		}
	}
	if p.Subjects != nil {
		if d.Subjects, err = cleanSet("subjects", *p.Subjects); err != nil {
			return d, err
		}
	}
	if p.Extra != nil {
		if d.Extra, err = cleanExtra(*p.Extra); err != nil {
			return d, err
		}
	}
	return d, nil
}

func (p ProjectPatch) apply(d ProjectDescription) (ProjectDescription, error) {
	var err error
	if p.Name != nil {
		if d.Name, err = cleanText("name", *p.Name, 200); err != nil {
			return d, err
		}
		if d.Name == "" {
			return d, invalid("name must not be empty")
		}
	}
	if p.ProjectType != nil {
		if !slices.Contains(ProjectTypes, *p.ProjectType) {
			return d, invalid("project_type must be one of %v", ProjectTypes)
		}
		d.ProjectType = *p.ProjectType
	}
	if p.Summary != nil {
		if d.Summary, err = cleanText("summary", *p.Summary, 4000); err != nil {
			return d, err
		}
	}
	if p.Extra != nil {
		if d.Extra, err = cleanExtra(*p.Extra); err != nil {
			return d, err
		}
	}
	return d, nil
}

func cleanExtra(in map[string]any) (map[string]any, error) {
	if len(in) == 0 {
		return nil, nil
	}
	raw, err := canonjson.CanonicalizeValue(in)
	if err != nil {
		return nil, invalid("extra must contain valid JSON values: %v", err)
	}
	tree, err := canonjson.Decode(raw)
	if err != nil {
		return nil, invalid("extra must contain valid JSON values: %v", err)
	}
	return tree.(map[string]any), nil
}

var assetOrder = []string{"contract", "asset_id", "project_id", "asset_type", "slug", "title", "summary", "tags", "subjects",
	"sensitivity", "defaults", "revision", "updated_by", "extra"}

var projectOrder = []string{"contract", "project_id", "key", "name", "project_type", "summary", "revision", "updated_by", "extra"}

func renderAsset(d AssetDescription) ([]byte, error) {
	type wire struct {
		Contract string `json:"contract"`
		AssetDescription
	}
	raw, err := manifest.Render(wire{Contract: AssetContract, AssetDescription: d}, func(p string) []string {
		if p == "" {
			return assetOrder
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if _, err := manifest.Decode(raw, AssetContract); err != nil {
		return nil, errcode.Wrap(errcode.SchemaInvalid, "the asset description does not satisfy "+AssetContract, err)
	}
	return raw, nil
}

func renderProject(d ProjectDescription) ([]byte, error) {
	type wire struct {
		Contract string `json:"contract"`
		ProjectDescription
	}
	raw, err := manifest.Render(wire{Contract: ProjectContract, ProjectDescription: d}, func(p string) []string {
		if p == "" {
			return projectOrder
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if _, err := manifest.Decode(raw, ProjectContract); err != nil {
		return nil, errcode.Wrap(errcode.SchemaInvalid, "the project description does not satisfy "+ProjectContract, err)
	}
	return raw, nil
}

func parseInto(raw []byte, contract string, v any) error {
	tree, err := manifest.Decode(raw, contract)
	if err != nil {
		return err
	}
	b, err := json.Marshal(tree)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// targetDir 返回说明所在目录与文件基名。
func (s *Service) targetDir(t commit.MetadataTarget) (dir, base string) {
	if t.Kind == commit.TargetProject {
		return s.layout.ProjectDir(t.ProjectID), "project"
	}
	return s.layout.AssetDir(t.ProjectID, t.ID), "asset"
}

// revisionPath 是某次操作写出的修订文件：修订号相同的未生效尝试（被取消的
// 操作）各有自己的文件，互不覆盖；生效的是台账记录的 (修订号, 操作)。
func (s *Service) revisionPath(t commit.MetadataTarget, revision int64, op ids.ID) string {
	dir, base := s.targetDir(t)
	return filepath.Join(dir, ".history", fmt.Sprintf("%s.r%06d.%s.yaml", base, revision, op))
}

func (s *Service) snapshotPath(t commit.MetadataTarget) string {
	dir, base := s.targetDir(t)
	return filepath.Join(dir, base+".yaml")
}

// writeRevision 以排他方式写出修订文件；同一操作重入时内容相同即幂等。
func (s *Service) writeRevision(ctx context.Context, p commit.PreparedMetadata, content []byte) (commit.RevisionProof, error) {
	if digest.Of(content) != p.ContentDigest {
		return commit.RevisionProof{}, errcode.New(errcode.OperationNeedsReconciliation, "the rendered description does not match the prepared revision")
	}
	_, release, err := s.write(ctx)
	if err != nil {
		return commit.RevisionProof{}, err
	}
	defer release()
	path := s.revisionPath(p.Target, p.Revision, p.OperationID)
	if _, err := s.fs.CreateOrMatch(path, content, 0o444); err != nil {
		if errors.Is(err, fileop.ErrContentDiffers) {
			return commit.RevisionProof{}, errcode.New(errcode.OperationNeedsReconciliation, "a different revision file already exists for this operation")
		}
		return commit.RevisionProof{}, err
	}
	return commit.RevisionProof{OperationID: p.OperationID, Target: p.Target, Revision: p.Revision,
		ContentDigest: p.ContentDigest, FileRef: s.ref(path), WrittenAt: s.now()}, nil
}

// VerifyRevision 实现 commit.RevisionVerifier：文件位置必须是该操作的修订文件，
// 内容的 SHA-256 与摘要一致。
func (s *Service) VerifyRevision(ctx context.Context, p commit.RevisionProof) error {
	path := s.revisionPath(p.Target, p.Revision, p.OperationID)
	if p.FileRef != s.ref(path) {
		return errcode.New(errcode.OperationNeedsReconciliation, "the proof does not point at this operation's revision file")
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return errcode.New(errcode.OperationNeedsReconciliation, "the revision file is missing")
	}
	if err != nil {
		return fileop.Wrap("reading a revision file", err)
	}
	if digest.Of(raw) != p.ContentDigest {
		return errcode.New(errcode.HashMismatch, "the revision file no longer matches its digest")
	}
	return ctx.Err()
}

// readCurrent 返回当前生效修订的文件内容；还没有修订时 raw 为 nil。
func (s *Service) readCurrent(ctx context.Context, t commit.MetadataTarget) ([]byte, commit.CommittedMetadata, error) {
	cur, err := s.ledger.CurrentMetadata(ctx, t)
	if errcode.CodeOf(err) == errcode.NotFound {
		return nil, commit.CommittedMetadata{}, nil
	}
	if err != nil {
		return nil, cur, err
	}
	raw, err := os.ReadFile(s.revisionPath(t, cur.Revision, cur.OperationID))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, cur, errcode.New(errcode.OperationNeedsReconciliation, "the current revision file is missing")
		}
		return nil, cur, fileop.Wrap("reading the current revision", err)
	}
	if digest.Of(raw) != cur.ContentDigest {
		return nil, cur, errcode.New(errcode.HashMismatch, "the current revision file no longer matches its digest")
	}
	return raw, cur, nil
}

// publishSnapshot 用当前修订替换可读快照（asset.yaml / project.yaml）。快照只供
// 阅读与对账，写入失败不影响已生效的修订；只在修订仍是当前时写入，不用旧
// 修订覆盖新快照。
func (s *Service) publishSnapshot(ctx context.Context, t commit.MetadataTarget, c commit.CommittedMetadata, content []byte) {
	locks := commands.Request{}
	if t.Kind == commit.TargetProject {
		locks.Projects = []string{string(t.ID)}
	} else {
		locks.Assets = []string{string(t.ID)}
	}
	lctx, held, err := s.gate.Acquire(ctx, locks)
	if err != nil {
		return
	}
	defer held.Release()
	// 所有快照发布先按目标串行化，再检查当前指针。否则旧请求检查通过后
	// 暂停，新请求发布新修订，旧请求恢复就会把新快照回退。
	cur, err := s.ledger.CurrentMetadata(lctx, t)
	if err != nil || cur.Revision != c.Revision || cur.OperationID != c.OperationID {
		return
	}
	_ = s.fs.ReplaceAtomic(s.snapshotPath(t), content, 0o644)
}

// revisionCommand 描述一次说明修订的命令。
type revisionCommand struct {
	who      authz.Context
	key      string
	target   commit.MetadataTarget
	expected int64
	action   authz.Action
	// body 是请求摘要所覆盖的规范化命令体（目标、预期修订与修改内容）。
	body any
	// opID 可选：由父操作派生的子操作 ID；为空时分配新 ID。
	opID ids.ID
	// render 按将要生效的修订号渲染新内容。
	render func(revision int64) ([]byte, error)
}

// commitRevision 执行说明修订：台账保留新修订号 → 排他写出修订文件 → 台账
// 最终接受。同一幂等键重放返回原结果；预期修订过期返回 PRECONDITION_FAILED。
func (s *Service) commitRevision(ctx context.Context, rc revisionCommand) (commit.CommittedMetadata, []byte, error) {
	bodyJSON, err := json.Marshal(rc.body)
	if err != nil {
		return commit.CommittedMetadata{}, nil, err
	}
	hash, err := commands.RequestHash(commands.HashInput{
		CommandType: commit.CommandCommitMetadata, ProjectID: rc.target.ProjectID, Targets: []string{string(rc.target.ID)},
		ExpectedRevisions: map[string]int64{string(rc.target.ID): rc.expected}, Body: bodyJSON,
	})
	if err != nil {
		return commit.CommittedMetadata{}, nil, err
	}
	op := rc.opID
	if op == "" {
		if op, err = s.ids.New(); err != nil {
			return commit.CommittedMetadata{}, nil, err
		}
	}
	cmd := commands.Context{OperationID: op, IdempotencyKey: rc.key, RequestHash: hash, CommandType: commit.CommandCommitMetadata,
		ActorID: rc.who.PrincipalID, SessionID: rc.who.SessionID, ProjectID: rc.target.ProjectID, RecoveryEpoch: rc.who.RecoveryEpoch,
		ExpectedRevisions: map[string]int64{string(rc.target.ID): rc.expected}}
	if err := cmd.Validate(); err != nil {
		return commit.CommittedMetadata{}, nil, invalid("invalid request: %v", err)
	}
	content, err := rc.render(rc.expected + 1)
	if err != nil {
		return commit.CommittedMetadata{}, nil, err
	}
	pm, err := s.ledger.PrepareMetadata(ctx, cmd, commit.MetadataRequest{Who: rc.who, Target: rc.target,
		ExpectedRevision: rc.expected, ContentDigest: digest.Of(content), Action: rc.action})
	if err != nil {
		return commit.CommittedMetadata{}, nil, err
	}
	var proof commit.RevisionProof
	if pm.ContentDigest == digest.Of(content) && pm.Revision == rc.expected+1 {
		if proof, err = s.writeRevision(ctx, pm, content); err != nil {
			return commit.CommittedMetadata{}, nil, err
		}
	} else {
		// 重放一个早已生效、之后又有新修订的操作：原修订文件已存在，按原操作取回。
		path := s.revisionPath(pm.Target, pm.Revision, pm.OperationID)
		if content, err = os.ReadFile(path); err != nil {
			return commit.CommittedMetadata{}, nil, errcode.New(errcode.OperationNeedsReconciliation, "the replayed revision file is missing")
		}
		if digest.Of(content) != pm.ContentDigest {
			return commit.CommittedMetadata{}, nil, errcode.New(errcode.HashMismatch, "the replayed revision file no longer matches its digest")
		}
		proof = commit.RevisionProof{OperationID: pm.OperationID, Target: pm.Target, Revision: pm.Revision,
			ContentDigest: pm.ContentDigest, FileRef: s.ref(path), WrittenAt: s.now()}
	}
	cm, err := s.ledger.CommitMetadata(ctx, pm.OperationID, rc.who, proof)
	if err != nil {
		return commit.CommittedMetadata{}, nil, err
	}
	s.publishSnapshot(ctx, rc.target, cm, content)
	return cm, content, nil
}

// defaultTitle 从路径别名推导默认标题（最后一段）。
func defaultTitle(slug string) string { return path.Base(slug) }
