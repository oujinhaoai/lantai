package catalog

import (
	"context"

	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// ReadProjection 是核心 query 重建投影的可信读取入口。它从台账取得当前说明
// 指针，再按摘要核对文件；不读取 asset.yaml 等可丢弃快照。本接口不做用户
// 授权，不得直接用于传输层；query 在每次响应时核对当前授权与来源限制。
func (s *Service) ReadProjection(ctx context.Context, assetID ids.ID) (AssetInfo, error) {
	a, err := s.ledger.Asset(ctx, assetID)
	if err != nil {
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
	return AssetInfo{Asset: a, Description: d, Claim: claim, Latest: latest}, nil
}

// ReadProjectionVersion 为核心 query 重建读取已提交版本的冻结清单；未提交
// 文件永不进入投影。与 ReadProjection 一样，此接口仅限核心可信调用。
func (s *Service) ReadProjectionVersion(ctx context.Context, assetID, versionID ids.ID) (VersionInfo, error) {
	v, err := s.ledger.Version(ctx, assetID, versionID)
	if err != nil {
		return VersionInfo{}, err
	}
	doc, err := s.readManifest(ctx, v)
	if err != nil {
		return VersionInfo{}, err
	}
	return VersionInfo{Version: v, Manifest: doc}, nil
}
