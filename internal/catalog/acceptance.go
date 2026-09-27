package catalog

import (
	"context"
	"slices"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

var _ commit.AcceptanceVerifier = (*Service)(nil)

// VerifyAcceptance 是台账最终接受边界内的内容与授权复验。台账调用时必须
// 已取得实例屏障共享锁与 security_guard 读锁，并保持到提交结束；本方法不
// 自行获取这些锁，不把早先的解析或安装结果当作当前授权。
func (s *Service) VerifyAcceptance(ctx context.Context, who authz.Context, p commit.Prepared) error {
	if who.PrincipalID != p.ActorID {
		return errcode.New(errcode.Forbidden, "only the accepting actor can complete this operation")
	}
	project, err := s.ledger.Project(ctx, p.ProjectID)
	if err != nil {
		return err
	}
	if project.State != commit.ProjectActive {
		return reasonErr(errcode.InvalidStateTransition, "project_not_active", "the project does not accept new versions")
	}
	action := commit.ActionCommitVersion
	if p.AliasGeneration > 0 {
		action = ActionCreateAsset
	}
	if err := s.authorize(ctx, who, action, authz.Resource{ProjectID: p.ProjectID, Kind: "project", ID: p.ProjectID}); err != nil {
		return err
	}
	content, err := s.thaw(p.OperationID, p.ManifestDigest)
	if err != nil {
		return err
	}
	if !slices.Equal(content.InstallFiles(), p.Files) {
		return errcode.New(errcode.OperationNeedsReconciliation, "the frozen manifest files do not match the ledger reservation")
	}
	for _, use := range content.Uses {
		ref := ids.PermanentRef{InstanceID: use.InstanceID, AssetID: use.AssetID, VersionID: use.VersionID}
		if _, err := s.ResolvePermanent(ctx, who, ref); err != nil {
			return err
		}
		decision, err := s.rights.EvaluateUse(ctx, who, ref, usePurpose(use.Relation, content.Rights.Usage))
		if err != nil {
			return err
		}
		if err := decision.Err(); err != nil {
			return err
		}
	}
	return s.storage.VerifyGrantAcceptance(ctx, who, p.InstallRequest(), usePurpose("uses", content.Rights.Usage))
}
