package catalog

import (
	"bytes"
	"context"
	"io"

	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// projectDescription 返回项目的当前说明；还没有修订时由登记事实推导（修订 0）。
func (s *Service) projectDescription(ctx context.Context, p commit.Project) (ProjectDescription, error) {
	target := commit.MetadataTarget{Kind: commit.TargetProject, ProjectID: p.ProjectID, ID: p.ProjectID}
	raw, _, err := s.readCurrent(ctx, target)
	if err != nil {
		return ProjectDescription{}, err
	}
	if raw == nil {
		return defaultProject(p), nil
	}
	var d ProjectDescription
	return d, parseInto(raw, ProjectContract, &d)
}

// derivedDescription 是还没有说明修订时的资产说明（修订 0）：类型、敏感级别
// 与默认许可取自首个版本的清单，标题取路径的最后一段。
func (s *Service) derivedDescription(ctx context.Context, a commit.Asset) (AssetDescription, error) {
	first, err := s.ledger.VersionByNumber(ctx, a.AssetID, 1)
	if err != nil {
		if errcode.CodeOf(err) != errcode.NotFound {
			return AssetDescription{}, err
		}
		// 版本号 1 可能被取消的操作占用而从未提交：取最早的已提交版本。
		if first, err = s.firstCommitted(ctx, a.AssetID); err != nil {
			return AssetDescription{}, err
		}
	}
	doc, err := s.readManifest(ctx, first)
	if err != nil {
		return AssetDescription{}, err
	}
	r := doc.Content.Rights
	return AssetDescription{AssetID: a.AssetID, ProjectID: a.ProjectID, AssetType: doc.Content.AssetType, Slug: a.Slug,
		Title: defaultTitle(a.Slug), Tags: []string{}, Subjects: []string{}, Sensitivity: r.Sensitivity,
		Defaults: &Defaults{Usage: r.Usage, License: r.License}}, nil
}

func (s *Service) firstCommitted(ctx context.Context, asset ids.ID) (commit.Committed, error) {
	latest, err := s.ledger.LatestVersion(ctx, asset)
	if err != nil {
		return commit.Committed{}, err
	}
	for n := int64(1); n <= latest.VersionNumber; n++ {
		if v, err := s.ledger.VersionByNumber(ctx, asset, n); err == nil {
			return v, nil
		} else if errcode.CodeOf(err) != errcode.NotFound {
			return commit.Committed{}, err
		}
	}
	return latest, nil
}

// assetDescription 返回资产的当前说明；还没有修订时由登记事实推导。
func (s *Service) assetDescription(ctx context.Context, a commit.Asset) (AssetDescription, error) {
	target := commit.MetadataTarget{Kind: commit.TargetAsset, ProjectID: a.ProjectID, ID: a.AssetID}
	raw, _, err := s.readCurrent(ctx, target)
	if err != nil {
		return AssetDescription{}, err
	}
	if raw == nil {
		return s.derivedDescription(ctx, a)
	}
	var d AssetDescription
	return d, parseInto(raw, AssetContract, &d)
}

// GetProject 按 key 读取项目登记与当前说明。
func (s *Service) GetProject(ctx context.Context, who authz.Context, key string) (Project, error) {
	ctx, held, err := s.gate.Coordinator().Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return Project{}, err
	}
	defer held.Release()
	p, err := s.ledger.ProjectByKey(ctx, key)
	if err != nil {
		return Project{}, notFoundIfMissing(err)
	}
	if err := s.canRead(ctx, who, p.ProjectID, "project", p.ProjectID); err != nil {
		return Project{}, err
	}
	d, err := s.projectDescription(ctx, p)
	if err != nil {
		return Project{}, err
	}
	return Project{Project: p, Description: d}, nil
}

type ProjectPage struct {
	Items      []Project `json:"items"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

// ListProjects 只枚举当前可读项目，游标不包含隐藏项目的 ID 或计数。
func (s *Service) ListProjects(ctx context.Context, who authz.Context, after ids.ID, limit int) (ProjectPage, error) {
	out := ProjectPage{Items: []Project{}}
	if limit < 1 || limit > 100 || (after != "" && !after.Valid()) {
		return out, invalid("invalid project pagination")
	}
	enumerator, ok := s.ledger.(interface {
		Projects(context.Context, ids.ID, int) ([]commit.Project, error)
	})
	if !ok {
		return out, errcode.New(errcode.Internal, "project enumeration is unavailable")
	}
	ctx, held, err := s.gate.Coordinator().Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return out, err
	}
	defer held.Release()
	for {
		batch, err := enumerator.Projects(ctx, after, 100)
		if err != nil {
			return out, err
		}
		for _, p := range batch {
			after = p.ProjectID
			decision, err := s.authz.Authorize(ctx, who, ActionRead, authz.Resource{ProjectID: p.ProjectID, Kind: "project", ID: p.ProjectID})
			if err != nil {
				return out, err
			}
			if !decision.Allowed {
				switch decision.Code {
				case errcode.AuthRequired, errcode.TokenExpired, errcode.TokenRevoked:
					return out, decision.Err()
				}
				continue
			}
			if len(out.Items) == limit {
				out.NextCursor = string(out.Items[len(out.Items)-1].ProjectID)
				return out, nil
			}
			d, err := s.projectDescription(ctx, p)
			if err != nil {
				return out, err
			}
			out.Items = append(out.Items, Project{Project: p, Description: d})
		}
		if len(batch) < 100 {
			return out, nil
		}
	}
}

// AssetInfo 是资产登记、当前说明、当前占名与最新版本。
type AssetInfo struct {
	Asset       commit.Asset     `json:"asset"`
	Description AssetDescription `json:"description"`
	// Claim 是资产建立时所取路径的当前占名；名称被释放并由新资产取得后，
	// Claim.AssetID 不再是本资产。
	Claim  commit.Claim     `json:"claim"`
	Latest commit.Committed `json:"latest"`
}

// GetAsset 读取资产。无权读取时 NOT_FOUND。
func (s *Service) GetAsset(ctx context.Context, who authz.Context, assetID ids.ID) (AssetInfo, error) {
	ctx, held, err := s.gate.Coordinator().Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return AssetInfo{}, err
	}
	defer held.Release()
	a, err := s.ledger.Asset(ctx, assetID)
	if err != nil {
		return AssetInfo{}, notFoundIfMissing(err)
	}
	if err := s.canRead(ctx, who, a.ProjectID, "asset", a.AssetID); err != nil {
		return AssetInfo{}, err
	}
	d, err := s.assetDescription(ctx, a)
	if err != nil {
		return AssetInfo{}, err
	}
	claim, err := s.ledger.Claim(ctx, a.ProjectID, a.Slug)
	if err != nil {
		return AssetInfo{}, err
	}
	latest, err := s.ledger.LatestVersion(ctx, a.AssetID)
	if err != nil {
		return AssetInfo{}, err
	}
	if err := s.canReadRights(ctx, who, latest); err != nil {
		return AssetInfo{}, err
	}
	if d.Sensitivity == "personal" {
		decision, err := s.authz.Authorize(ctx, who, "personal.read", authz.Resource{ProjectID: a.ProjectID, Kind: "asset", ID: a.AssetID})
		if err != nil {
			return AssetInfo{}, err
		}
		if !decision.Allowed {
			return AssetInfo{}, errcode.New(errcode.NotFound, "")
		}
	}
	return AssetInfo{Asset: a, Description: d, Claim: claim, Latest: latest}, nil
}

// VersionInfo 是已提交版本的登记与冻结清单。
type VersionInfo struct {
	Version  commit.Committed  `json:"version"`
	Manifest manifest.Document `json:"manifest"`
}

// GetVersion 精确读取已提交版本及其清单（不依赖检索索引）。版本不属于资产时
// REF_MISMATCH；无权读取时 NOT_FOUND。
func (s *Service) GetVersion(ctx context.Context, who authz.Context, assetID, versionID ids.ID) (VersionInfo, error) {
	ctx, held, err := s.gate.Coordinator().Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return VersionInfo{}, err
	}
	defer held.Release()
	a, err := s.ledger.Asset(ctx, assetID)
	if err != nil {
		return VersionInfo{}, notFoundIfMissing(err)
	}
	if err := s.canRead(ctx, who, a.ProjectID, "asset", a.AssetID); err != nil {
		return VersionInfo{}, err
	}
	v, err := s.ledger.Version(ctx, assetID, versionID)
	if err != nil {
		return VersionInfo{}, err
	}
	if err := s.canReadRights(ctx, who, v); err != nil {
		return VersionInfo{}, err
	}
	doc, err := s.readManifest(ctx, v)
	if err != nil {
		return VersionInfo{}, err
	}
	return VersionInfo{Version: v, Manifest: doc}, nil
}

func (s *Service) canReadRights(ctx context.Context, who authz.Context, v commit.Committed) error {
	d, err := s.rights.EvaluateUse(ctx, who, v.Ref(s.instance), authz.PurposeArchiveReview)
	if err != nil {
		return err
	}
	if !d.Allowed {
		return errcode.New(errcode.NotFound, "")
	}
	return nil
}
