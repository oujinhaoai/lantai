package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/oujinhaoai/lantai/internal/catalog/pathrule"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
	"github.com/oujinhaoai/lantai/internal/storage/fileop"
)

// 版本选择。
const (
	SelectPublished = "published"
	SelectApproved  = "approved"
	SelectLatest    = "latest"
)

// Ref 是解析后的可读引用 <项目key>/<路径>@<选择>。
type Ref struct {
	ProjectKey string
	Slug       string
	// Selector 是 published、approved、latest，或固定版本 vNNN。
	Selector string
	// Number 在固定版本时为版本号。
	Number int64
}

// Floating 报告引用是否为浮动导航（非固定版本号）。
func (r Ref) Floating() bool { return r.Number == 0 }

func (r Ref) String() string { return r.ProjectKey + "/" + r.Slug + "@" + r.Selector }

var versionSelRE = regexp.MustCompile(`^v([0-9]{1,9})$`)

// ParseRef 解析可读引用。馆际引用（<馆名>::…）属于 M8，明确拒绝。
func ParseRef(s string) (Ref, error) {
	if strings.Contains(s, "::") {
		return Ref{}, reasonErr(errcode.SchemaInvalid, "federation_unsupported", "references to other instances are not supported yet")
	}
	at := strings.LastIndexByte(s, '@')
	if at < 0 {
		return Ref{}, reasonErr(errcode.SchemaInvalid, "selector_missing", "a reference needs @published, @approved, @latest or @vNNN")
	}
	pathPart, sel := s[:at], s[at+1:]
	key, slug, ok := strings.Cut(pathPart, "/")
	if !ok {
		return Ref{}, reasonErr(errcode.SchemaInvalid, "ref_path", "a reference is <project>/<path>@<version>")
	}
	if err := pathrule.CheckProjectKey(key); err != nil {
		return Ref{}, err
	}
	norm, err := pathrule.NormalizeSlug(slug)
	if err != nil {
		return Ref{}, err
	}
	r := Ref{ProjectKey: key, Slug: norm, Selector: sel}
	switch sel {
	case SelectPublished, SelectApproved, SelectLatest:
		return r, nil
	}
	m := versionSelRE.FindStringSubmatch(sel)
	if m == nil {
		return Ref{}, reasonErr(errcode.SchemaInvalid, "selector", fmt.Sprintf("unknown version selector %q", sel))
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	if n < 1 {
		return Ref{}, reasonErr(errcode.SchemaInvalid, "selector", "version numbers start at v001")
	}
	r.Number = n
	return r, nil
}

// Resolved 是解析结果：具体的永久引用与解析时的占名状态。
type Resolved struct {
	Ref           ids.PermanentRef `json:"ref"`
	URI           string           `json:"uri"`
	ProjectID     ids.ID           `json:"project_id"`
	AssetID       ids.ID           `json:"asset_id"`
	VersionID     ids.ID           `json:"version_id"`
	VersionNumber int64            `json:"version_number"`
	Slug          string           `json:"slug"`
	Generation    int64            `json:"alias_generation"`
	Selector      string           `json:"selector"`
	// ClaimRevision 是解析时占名记录的修订。
	ClaimRevision int64 `json:"claim_revision"`
}

func (s *Service) resolved(c commit.Committed, slug string, generation, claimRev int64, selector string) Resolved {
	ref := c.Ref(s.instance)
	return Resolved{Ref: ref, URI: ids.URIScheme + "://" + string(ref.InstanceID) + "/assets/" + string(ref.AssetID) + "/versions/" + string(ref.VersionID),
		ProjectID: c.ProjectID, AssetID: c.AssetID, VersionID: c.VersionID, VersionNumber: c.VersionNumber,
		Slug: slug, Generation: generation, Selector: selector, ClaimRevision: claimRev}
}

// Resolve 解析可读引用。generation 为 0 表示未指定代次：路径从未被复用时
// 固定版本可以直接解析，复用过则返回 REF_AMBIGUOUS；浮动引用只解析当前
// 代次，不接受代次参数。调用者须能读取项目，否则一律 NOT_FOUND。
func (s *Service) Resolve(ctx context.Context, who authz.Context, ref string, generation int64) (Resolved, error) {
	if strings.HasPrefix(ref, ids.URIScheme+"://") {
		if generation != 0 {
			return Resolved{}, invalid("alias_generation does not apply to a permanent reference")
		}
		permanent, err := ids.ParseURI(ref)
		if err != nil {
			return Resolved{}, invalid("invalid permanent reference: %v", err)
		}
		return s.ResolvePermanent(ctx, who, permanent)
	}
	r, err := ParseRef(ref)
	if err != nil {
		return Resolved{}, err
	}
	if generation < 0 {
		return Resolved{}, invalid("alias_generation must be positive")
	}
	if generation > 0 && r.Floating() {
		return Resolved{}, reasonErr(errcode.SchemaInvalid, "generation_with_floating",
			"alias_generation only applies to fixed versions (@vNNN); floating references use the current generation")
	}
	project, err := s.ledger.ProjectByKey(ctx, r.ProjectKey)
	if err != nil {
		return Resolved{}, notFoundIfMissing(err)
	}
	if err := s.canRead(ctx, who, project.ProjectID, "project", project.ProjectID); err != nil {
		return Resolved{}, err
	}
	claim, err := s.ledger.Claim(ctx, project.ProjectID, r.Slug)
	if err != nil {
		return Resolved{}, notFoundIfMissing(err)
	}
	if r.Floating() {
		if claim.State != commit.ClaimActive {
			return Resolved{}, errcode.New(errcode.NotFound, "")
		}
		v, err := s.selectVersion(ctx, claim.AssetID, r.Selector)
		if err != nil {
			return Resolved{}, err
		}
		return s.resolved(v, claim.Slug, claim.Generation, claim.Revision, r.Selector), nil
	}
	asset := claim.AssetID
	gen := claim.Generation
	switch {
	case generation == 0 && claim.Generation > 1:
		return Resolved{}, errcode.New(errcode.RefAmbiguous, "this path has been reused; give alias_generation or a permanent reference").
			WithDetails(errcode.Detail{Reason: "path_reused", Data: map[string]any{"generations": claim.Generation}})
	case generation == 0, generation == claim.Generation:
	case generation < claim.Generation:
		rec, err := s.aliasRecord(ctx, project.ProjectID, r.Slug, generation)
		if err != nil {
			return Resolved{}, err
		}
		asset, gen = rec.AssetID, generation
	default:
		return Resolved{}, errcode.New(errcode.NotFound, "")
	}
	v, err := s.ledger.VersionByNumber(ctx, asset, r.Number)
	if err != nil {
		return Resolved{}, notFoundIfMissing(err)
	}
	return s.resolved(v, r.Slug, gen, claim.Revision, r.Selector), nil
}

func (s *Service) selectVersion(ctx context.Context, asset ids.ID, selector string) (commit.Committed, error) {
	var id ids.ID
	var err error
	switch selector {
	case SelectLatest:
		return s.ledger.LatestVersion(ctx, asset)
	case SelectPublished:
		if s.pubs == nil {
			return commit.Committed{}, errcode.New(errcode.NotPublished, "")
		}
		id, err = s.pubs.Published(ctx, asset)
	case SelectApproved:
		if s.pubs == nil {
			return commit.Committed{}, reasonErr(errcode.NotFound, "no_approved_version", "the asset has no approved version")
		}
		id, err = s.pubs.Approved(ctx, asset)
	default:
		return commit.Committed{}, invalid("unknown selector %q", selector)
	}
	if err != nil {
		return commit.Committed{}, err
	}
	return s.ledger.Version(ctx, asset, id)
}

// ResolvePermanent 精确解析永久引用：版本必须属于该资产（REF_MISMATCH），无权
// 读取时 NOT_FOUND。别的馆的引用（M8）明确拒绝。
func (s *Service) ResolvePermanent(ctx context.Context, who authz.Context, ref ids.PermanentRef) (Resolved, error) {
	if ref.InstanceID != "" && ref.InstanceID != s.instance {
		return Resolved{}, reasonErr(errcode.SchemaInvalid, "federation_unsupported", "references to other instances are not supported yet")
	}
	if !ref.AssetID.Valid() || !ref.VersionID.Valid() {
		return Resolved{}, errcode.New(errcode.NotFound, "")
	}
	asset, err := s.ledger.Asset(ctx, ref.AssetID)
	if err != nil {
		return Resolved{}, notFoundIfMissing(err)
	}
	if err := s.canRead(ctx, who, asset.ProjectID, "asset", asset.AssetID); err != nil {
		return Resolved{}, err
	}
	v, err := s.ledger.Version(ctx, ref.AssetID, ref.VersionID)
	if err != nil {
		return Resolved{}, err
	}
	return s.resolved(v, asset.Slug, asset.Generation, 0, "v"+fmt.Sprintf("%03d", v.VersionNumber)), nil
}

func notFoundIfMissing(err error) error {
	if errcode.CodeOf(err) == errcode.NotFound {
		return errcode.New(errcode.NotFound, "")
	}
	return err
}

// 别名历史：每一代一个不可变的 lantai.alias-generation/v1 文件，按路径折叠键的
// 摘要分目录。当前代次由台账的占名记录回答；历史文件用于解析旧代次，并可在
// 库丢失时重建映射。孤立的历史文件（其操作从未提交）不能抢占现名：解析旧代次
// 时仍核对该资产在台账中已提交。

// AliasRecord 是别名的一代（lantai.alias-generation/v1）。
type AliasRecord struct {
	ProjectID      ids.ID `json:"project_id"`
	NormalizedSlug string `json:"normalized_slug"`
	Generation     int64  `json:"generation"`
	AssetID        ids.ID `json:"asset_id"`
	CreatedAt      string `json:"created_at"`
	Reason         string `json:"reason"`
	OperationID    ids.ID `json:"operation_id,omitempty"`
	ReleasedAt     string `json:"released_at,omitempty"`
	ReleaseReason  string `json:"release_reason,omitempty"`
}

func (s *Service) aliasDir(project ids.ID, slug string) string {
	sum := sha256.Sum256([]byte(pathrule.Key(slug)))
	return filepath.Join(s.layout.ProjectDir(project), "namespace", hex.EncodeToString(sum[:16]))
}

func (s *Service) aliasPath(project ids.ID, slug string, generation int64) string {
	return filepath.Join(s.aliasDir(project, slug), fmt.Sprintf("g%06d.json", generation))
}

// writeAliasRecord 写出资产建立时取得的代次记录；重入幂等。
func (s *Service) writeAliasRecord(ctx context.Context, a commit.Asset) error {
	rec := AliasRecord{ProjectID: a.ProjectID, NormalizedSlug: a.Slug, Generation: a.Generation, AssetID: a.AssetID,
		CreatedAt: clock.Format(a.CreatedAt), Reason: "create", OperationID: a.OperationID}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	reg, err := schema.Default()
	if err != nil {
		return err
	}
	if err := reg.ValidateJSON("lantai.alias-generation/v1", raw); err != nil {
		return err
	}
	_, release, err := s.write(ctx)
	if err != nil {
		return err
	}
	defer release()
	if _, err := s.fs.CreateOrMatch(s.aliasPath(a.ProjectID, a.Slug, a.Generation), append(raw, '\n'), 0o444); err != nil {
		if errors.Is(err, fileop.ErrContentDiffers) {
			return errcode.New(errcode.OperationNeedsReconciliation, "a different alias record already exists for this generation")
		}
		return err
	}
	return nil
}

// aliasRecord 读取某一代的记录，并核对其资产在台账中已提交。
func (s *Service) aliasRecord(ctx context.Context, project ids.ID, slug string, generation int64) (AliasRecord, error) {
	raw, err := os.ReadFile(s.aliasPath(project, slug, generation))
	if errors.Is(err, fs.ErrNotExist) {
		return AliasRecord{}, errcode.New(errcode.OperationNeedsReconciliation, "the alias history for this generation is missing").
			WithDetails(errcode.Detail{Reason: "alias_history_missing"})
	}
	if err != nil {
		return AliasRecord{}, fileop.Wrap("reading alias history", err)
	}
	var rec AliasRecord
	if err := json.Unmarshal(raw, &rec); err != nil || rec.Generation != generation || rec.ProjectID != project ||
		pathrule.Key(rec.NormalizedSlug) != pathrule.Key(slug) {
		return AliasRecord{}, errcode.New(errcode.OperationNeedsReconciliation, "the alias history record is inconsistent")
	}
	a, err := s.ledger.Asset(ctx, rec.AssetID)
	if err != nil || a.Generation != generation || a.ProjectID != project {
		return AliasRecord{}, errcode.New(errcode.NotFound, "")
	}
	if pathrule.Key(a.Slug) != pathrule.Key(slug) || a.OperationID != rec.OperationID || clock.Format(a.CreatedAt) != rec.CreatedAt {
		return AliasRecord{}, errcode.New(errcode.OperationNeedsReconciliation, "the alias history record does not match its ledger registration")
	}
	return rec, nil
}

// RepairReport 是别名历史对账的结果。
type RepairReport struct {
	Checked int `json:"checked"`
	Written int `json:"written"`
}

// RepairAliasHistory 按台账的已提交资产补齐缺失的别名历史文件（例如提交后、
// 写历史前进程退出）。只补写与台账一致的记录，不凭文件改动台账。
func (s *Service) RepairAliasHistory(ctx context.Context) (RepairReport, error) {
	var rep RepairReport
	var after ids.ID
	for {
		page, err := s.ledger.Versions(ctx, after, 500)
		if err != nil {
			return rep, err
		}
		if len(page) == 0 {
			return rep, nil
		}
		for _, v := range page {
			after = v.VersionID
			if v.AliasGeneration == 0 {
				continue
			}
			a, err := s.ledger.Asset(ctx, v.AssetID)
			if err != nil {
				return rep, err
			}
			rep.Checked++
			if _, err := os.Stat(s.aliasPath(a.ProjectID, a.Slug, a.Generation)); err == nil {
				if _, err := s.aliasRecord(ctx, a.ProjectID, a.Slug, a.Generation); err != nil {
					return rep, err
				}
				continue
			} else if !errors.Is(err, fs.ErrNotExist) {
				return rep, fileop.Wrap("checking alias history", err)
			}
			if err := s.writeAliasRecord(ctx, a); err != nil {
				return rep, err
			}
			rep.Written++
		}
	}
}
