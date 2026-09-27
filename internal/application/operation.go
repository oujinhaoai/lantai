package application

import (
	"context"
	"errors"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity"
)

// Operation 仅开放本人命令的精简状态。当前权限和来源限制在同一个安全边界
// 内复核；结果不含原响应正文（其中可能有一次性凭据），也不暴露内部路径。
func (a *App) Operation(ctx context.Context, who authz.Context, id ids.ID) (*commands.View, error) {
	if !id.Valid() {
		return nil, errcode.New(errcode.SchemaInvalid, "invalid operation identifier")
	}
	ctx, h, err := a.Instance.Gate().Coordinator().Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return nil, err
	}
	defer h.Release()
	current, err := a.Identity.WhoAmI(ctx, who)
	if err != nil {
		return nil, err
	}
	who = current.Context
	// 同一提交在 runtime 中有上传、ledger 中有最终提交；优先展示台账回执。
	for _, db := range []ownership.Database{ownership.Ledger, ownership.Runtime, ownership.Main} {
		r, v, e := (commands.OperationSource{DB: a.Instance.DB(db)}).Lookup(ctx, id)
		if errors.Is(e, commands.ErrNotFound) {
			continue
		}
		if e != nil {
			return nil, e
		}
		if r.Key.ActorID != who.PrincipalID {
			return nil, errcode.New(errcode.NotFound, "")
		}
		if r.Key.ProjectID != "" {
			d, e := a.Identity.Authorize(ctx, who, identity.ActCatalogRead, authz.Resource{ProjectID: r.Key.ProjectID, Kind: "project", ID: r.Key.ProjectID})
			if e != nil {
				return nil, e
			}
			if !d.Allowed {
				return nil, errcode.New(errcode.NotFound, "")
			}
		}
		refs := make([]commands.ResultRef, 0, len(v.ResultRefs))
		for _, ref := range v.ResultRefs {
			allowed, e := a.visibleResult(ctx, who, ref)
			if e != nil {
				return nil, e
			}
			if allowed {
				ref.URI = "" // 不转发任意持久化 URI；客户端通过已授权类型与 ID 精确读取。
				refs = append(refs, ref)
			}
		}
		v.ResultRefs = refs
		v.ParentOperationID = "" // 没有单独校验的关联 operation 不向客户端投影。
		return v, nil
	}
	return nil, errcode.New(errcode.NotFound, "")
}

func (a *App) visibleResult(ctx context.Context, who authz.Context, ref commands.ResultRef) (bool, error) {
	switch ref.Kind {
	case "project":
		p, err := a.Ledger.Project(ctx, ref.ID)
		if err != nil {
			return invisibleOrError(err)
		}
		d, err := a.Identity.Authorize(ctx, who, identity.ActCatalogRead, authz.Resource{ProjectID: p.ProjectID, Kind: "project", ID: p.ProjectID})
		return d.Allowed, err
	case "asset":
		p, err := a.Catalog.ReadProjection(ctx, ref.ID)
		if err != nil {
			return invisibleOrError(err)
		}
		d, err := a.Rights.EvaluateUse(ctx, who, p.Latest.Ref(a.Instance.InstanceID()), authz.PurposeArchiveReview)
		return d.Allowed, err
	case "version":
		v, err := a.Ledger.VersionByID(ctx, ref.ID)
		if err != nil {
			return invisibleOrError(err)
		}
		d, err := a.Rights.EvaluateUse(ctx, who, v.Ref(a.Instance.InstanceID()), authz.PurposeArchiveReview)
		return d.Allowed, err
	case "upload":
		_, err := a.Storage.GetUpload(ctx, who, ref.ID)
		if err != nil {
			return invisibleOrError(err)
		}
		return true, nil
	default:
		// 未接入可见性检查的对象默认隐藏，不能凭创建时的授权回放。
		return false, nil
	}
}

func invisibleOrError(err error) (bool, error) {
	switch errcode.CodeOf(err) {
	case errcode.NotFound, errcode.Forbidden, errcode.UseRestricted:
		return false, nil
	}
	return false, err
}
