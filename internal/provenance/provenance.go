// Package provenance 从不可变版本清单和已接受的追加证据计算当前用途限制。
// 不以索引、同哈希的最宽许可或普通著录替换历史证据；人审更正追加不可变断言。
package provenance

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/rights"
	"github.com/oujinhaoai/lantai/internal/storage"
)

const ActionAppendEvidence authz.Action = "provenance.append_evidence"
const ActionPersonalRead authz.Action = "personal.read"

// Files 是 T02 的权威文件适配器。SetFiles 只用于启动时解决组装依赖。
type Files interface {
	ReadManifest(context.Context, commit.Committed) ([]byte, error)
	AppendRecord(context.Context, storage.Record) (storage.RecordRef, error)
	ReadRecord(context.Context, ids.ID, ids.ID, ids.ID, ids.ID) ([]byte, error)
}

// Catalog 提供当前资产著录的可信读取。当前 personal 收紧必须约束下载和
// 来源复用，不能仅检查版本提交时的许可快照。接口未接线时用途判定 fail closed。
type Catalog interface {
	ReadProjection(context.Context, ids.ID) (catalog.AssetInfo, error)
}

// ProducerVerifier 由 T09 静态登记提供；没有登记时拒绝自报处理器身份。
// 仅检查身份，不启动插件，也不把处理器建议当作限制解除。
type ProducerVerifier interface {
	VerifyProducer(context.Context, storage.Producer) error
}
type Deps struct {
	DB                 *sql.DB
	Gate               *commands.Gate
	Reader             commit.Reader
	Files              Files
	Catalog            Catalog
	Authz              authz.Authorizer
	InstanceID         ids.ID
	Clock              clock.Clock
	IDs                *ids.Generator
	Producers          ProducerVerifier
	MaxDepth, MaxNodes int
}
type Service struct {
	Deps
	store *commands.Store
}

func New(d Deps) (*Service, error) {
	if d.DB == nil || d.Gate == nil || d.Reader == nil || d.Authz == nil || !d.InstanceID.Valid() || d.IDs == nil {
		return nil, errors.New("provenance: database, gate, reader, authorizer, instance and IDs are required")
	}
	if d.Clock == nil {
		d.Clock = clock.System{}
	}
	if d.MaxDepth <= 0 {
		d.MaxDepth = 64
	}
	if d.MaxNodes <= 0 {
		d.MaxNodes = 4096
	}
	st, err := commands.NewStore("provenance", d.Clock)
	if err != nil {
		return nil, err
	}
	return &Service{Deps: d, store: st}, nil
}
func (s *Service) SetFiles(f Files) { s.Files = f }

// SetCatalog 仅在开放实例前完成组装；运行中不可替换。
func (s *Service) SetCatalog(c Catalog) { s.Catalog = c }
func (s *Service) read(ctx context.Context, ref ids.PermanentRef) (commit.Committed, manifest.Document, error) {
	if err := ref.Validate(true); err != nil {
		return commit.Committed{}, manifest.Document{}, errcode.New(errcode.NotFound, "")
	}
	if ref.InstanceID != s.InstanceID || s.Files == nil {
		return commit.Committed{}, manifest.Document{}, errcode.New(errcode.RightsPending, "")
	}
	v, err := s.Reader.Version(ctx, ref.AssetID, ref.VersionID)
	if err != nil {
		return v, manifest.Document{}, err
	}
	raw, err := s.Files.ReadManifest(ctx, v)
	if err != nil {
		return v, manifest.Document{}, err
	}
	d, err := manifest.Parse(raw)
	if err != nil {
		return v, d, err
	}
	if d.InstanceID != ref.InstanceID || d.AssetID != v.AssetID || d.VersionID != v.VersionID || d.ProjectID != v.ProjectID || d.OperationID != v.OperationID || d.ManifestDigest != v.ManifestDigest || d.VersionNumber != v.VersionNumber || d.CreatedBy != v.CommittedBy {
		return v, d, errcode.New(errcode.HashMismatch, "manifest identity does not match the committed version")
	}
	return v, d, nil
}
func (s *Service) allowed(ctx context.Context, who authz.Context, action authz.Action, v commit.Committed) (bool, error) {
	d, err := s.Authz.Authorize(ctx, who, action, authz.Resource{ProjectID: v.ProjectID, Kind: "asset", ID: v.AssetID})
	return d.Allowed, err
}
func pending(epoch int64) rights.Decision {
	return rights.Decision{Code: errcode.RightsPending, RightsEpoch: epoch, RetryAfter: time.Minute}
}
func denied(epoch int64) rights.Decision {
	return rights.Decision{Code: errcode.UseRestricted, RightsEpoch: epoch}
}

// EvaluateUse 必须在调用者的最终 security_guard 边界内使用。它不再次取锁，
// 每次遍历权威证据和当前来源权限；预算/缺失/未知均不截断后放行。
func (s *Service) EvaluateUse(ctx context.Context, who authz.Context, ref ids.PermanentRef, purpose authz.Purpose) (rights.Decision, error) {
	switch purpose {
	case authz.PurposeArchiveReview, authz.PurposeReference, authz.PurposeProduction, authz.PurposeGenerativeInput, authz.PurposeRawExport:
	default:
		return denied(1), nil
	}
	return s.evaluate(ctx, who, ref, purpose, false, 0, map[ids.PermanentRef]bool{}, map[ids.PermanentRef]rights.Decision{})
}

// EvaluateRiskAccess is for trusted core risk/lifecycle callers, not downloads.
func (s *Service) EvaluateRiskAccess(ctx context.Context, who authz.Context, ref ids.PermanentRef) (rights.Decision, error) {
	return s.evaluate(ctx, who, ref, authz.PurposeArchiveReview, true, 0, map[ids.PermanentRef]bool{}, map[ids.PermanentRef]rights.Decision{})
}
func (s *Service) evaluate(ctx context.Context, who authz.Context, ref ids.PermanentRef, purpose authz.Purpose, risk bool, depth int, active map[ids.PermanentRef]bool, memo map[ids.PermanentRef]rights.Decision) (rights.Decision, error) {
	if err := ctx.Err(); err != nil {
		return rights.Decision{}, err
	}
	if active[ref] || depth > s.MaxDepth || len(memo)+len(active) >= s.MaxNodes {
		return pending(1), nil
	}
	if d, ok := memo[ref]; ok {
		return d, nil
	}
	active[ref] = true
	defer delete(active, ref)
	v, doc, err := s.read(ctx, ref)
	if err != nil {
		return pending(1), nil
	}
	ok, err := s.allowed(ctx, who, "catalog.read", v)
	if err != nil {
		return rights.Decision{}, err
	}
	if !ok {
		return denied(1), nil
	}
	if controls, ok := s.Reader.(commit.Controls); ok {
		var controlErr error
		if risk {
			if riskControls, ok := s.Reader.(commit.RiskControls); ok {
				controlErr = riskControls.CheckRiskRead(ctx, v.AssetID, v.VersionID)
			} else {
				controlErr = controls.CheckEvidenceAppend(ctx, v.AssetID, v.VersionID)
			}
		} else {
			controlErr = controls.CheckVersionRead(ctx, v.AssetID, v.VersionID)
		}
		if controlErr != nil {
			return denied(1), nil
		}
	}
	effective, err := s.assertionState(ctx, v, doc)
	if err != nil {
		return pending(1), nil
	}
	epoch, unknown, err := s.evidenceState(ctx, v, effective.Confirmed)
	if err != nil {
		return pending(1), nil
	}
	if s.Catalog == nil {
		return pending(epoch), nil
	}
	asset, err := s.Catalog.ReadProjection(ctx, v.AssetID)
	if err != nil || asset.Asset.AssetID != v.AssetID || asset.Asset.ProjectID != v.ProjectID {
		return pending(epoch), nil
	}
	r := effective.Rights
	if r.Sensitivity == "personal" || asset.Description.Sensitivity == "personal" {
		ok, err = s.allowed(ctx, who, ActionPersonalRead, v)
		if err != nil {
			return rights.Decision{}, err
		}
		if !ok {
			return denied(epoch), nil
		}
	}
	result := rights.Decision{Allowed: true, RightsEpoch: epoch}
	if purpose != authz.PurposeArchiveReview {
		if unknown || unknownLicense(r.License) {
			result = pending(epoch)
		}
		if effective.RestrictedSource || r.Usage == "restricted" || (r.Usage == "reference" && purpose != authz.PurposeReference) || (r.NoAI && purpose == authz.PurposeGenerativeInput) || (!r.RedistributeRaw && purpose == authz.PurposeRawExport) {
			result = denied(epoch)
		}
	}
	if slices.Contains(effective.DenyPurposes, purpose) && !risk {
		result = denied(epoch)
	}
	for _, u := range effective.Uses {
		// M1 尚无可放宽 reference 的 Profile，因此三类关系均保守继承限制。
		child, err := s.evaluate(ctx, who, ids.PermanentRef{InstanceID: u.InstanceID, AssetID: u.AssetID, VersionID: u.VersionID}, purpose, risk, depth+1, active, memo)
		if err != nil {
			return rights.Decision{}, err
		}
		result.RightsEpoch = max(result.RightsEpoch, child.RightsEpoch)
		if !child.Allowed && (result.Allowed || child.Code == errcode.UseRestricted) {
			result.Allowed = false
			result.Code = child.Code
			result.RetryAfter = child.RetryAfter
		}
	}
	memo[ref] = result
	return result, nil
}
func unknownLicense(s string) bool {
	for _, part := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return r == ' ' || r == '(' || r == ')' }) {
		if part == "none" || part == "noassertion" || strings.Contains(part, "unknown") {
			return true
		}
	}
	return strings.TrimSpace(s) == ""
}

// VisibleUses 只返回调用者此刻可读的固定来源引用。不可见来源无数量或占位泄露。
func (s *Service) VisibleUses(ctx context.Context, who authz.Context, ref ids.PermanentRef) ([]manifest.Use, error) {
	d, err := s.EvaluateUse(ctx, who, ref, authz.PurposeArchiveReview)
	if err != nil {
		return nil, err
	}
	if !d.Allowed {
		return nil, errcode.New(errcode.NotFound, "")
	}
	uses, err := s.EffectiveUses(ctx, ref.AssetID, ref.VersionID)
	if err != nil {
		return nil, err
	}
	out := []manifest.Use{}
	for _, u := range uses {
		d, err := s.EvaluateUse(ctx, who, ids.PermanentRef{InstanceID: u.InstanceID, AssetID: u.AssetID, VersionID: u.VersionID}, authz.PurposeArchiveReview)
		if err != nil {
			return nil, err
		}
		if d.Allowed {
			out = append(out, u)
		}
	}
	return out, nil
}

// HashMatches 保留调用者可见的全部候选；绝不自动选许可最宽的来源。
type HashMatches struct {
	Candidates         []ids.PermanentRef
	Ambiguous, Pending bool
}

func (s *Service) MatchHash(ctx context.Context, who authz.Context, sha string) (HashMatches, error) {
	out := HashMatches{Candidates: []ids.PermanentRef{}}
	if !digest.ValidHex(sha) {
		return out, errcode.New(errcode.SchemaInvalid, "invalid sha256")
	}
	after := ids.ID("")
	n := 0
	for {
		vs, err := s.Reader.Versions(ctx, after, 100)
		if err != nil {
			return out, err
		}
		if len(vs) == 0 {
			break
		}
		for _, v := range vs {
			after = v.VersionID
			n++
			if n > s.MaxNodes {
				out.Pending = true
				out.Ambiguous = len(out.Candidates) > 1
				return out, nil
			}
			ok, err := s.allowed(ctx, who, "catalog.read", v)
			if err != nil {
				return out, err
			}
			if !ok {
				continue
			}
			ref := v.Ref(s.InstanceID)
			decision, err := s.EvaluateUse(ctx, who, ref, authz.PurposeArchiveReview)
			if err != nil {
				return out, err
			}
			if !decision.Allowed {
				if decision.Code == errcode.RightsPending {
					out.Pending = true
				}
				continue
			}
			_, doc, err := s.read(ctx, ref)
			if err != nil {
				out.Pending = true
				continue
			}
			for _, f := range doc.Content.Files {
				if f.SHA256 == sha {
					out.Candidates = append(out.Candidates, ref)
					break
				}
			}
		}
	}
	out.Ambiguous = len(out.Candidates) > 1
	return out, nil
}
