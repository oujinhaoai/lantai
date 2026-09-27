// Package catalog 实现 T02 的资源目录端：项目与资产的说明修订（project.yaml、
// asset.yaml）、路径别名代次与引用解析、版本清单的规范化与冻结，以及把上传、
// 台账保留、文件安装与台账提交串成一次“入藏”的组装（T02.1）。
//
// catalog 没有自己的数据库表：说明与别名历史是数据根下的文件（内容真源），
// 哪个修订生效、路径由谁占用、版本是否提交由台账（commit 接口）裁决，字节由
// storage 管理。说明修订按 expected_revision 条件写：先由台账保留新修订号，
// 再以排他方式写出不可变的修订文件，最后由台账在最终接受边界复验并切换当前
// 修订；旧修订保留在 .history/ 中，不原地改写半个 YAML。
//
// 引用规则：永久引用（asset_id + version_id）精确寻址；path@vNNN 在路径被复用
// 过（代次大于 1）时必须带代次，否则 REF_AMBIGUOUS；浮动引用（@latest 等）
// 只解析当前代次并返回具体的永久引用。无权读取时不泄露对象是否存在。
package catalog

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/rights"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/fileop"
)

// Module 是本模块在所有权登记中的名称。
const Module = "catalog"

// 本模块使用的授权动作，登记在 identity 的权限矩阵中。
const (
	ActionRead             authz.Action = "catalog.read"
	ActionCreateAsset      authz.Action = "catalog.create_asset"
	ActionPatchMetadata    authz.Action = "catalog.patch_metadata"
	ActionPatchOwnMetadata authz.Action = "catalog.patch_own_metadata"
	ActionCreateProject    authz.Action = commit.ActionCreateProject
	ActionPatchProject     authz.Action = "catalog.patch_project"
)

// Publications 回答 @published 与 @approved（M2 由台账的审定与发布实现）。
// 未接入时 @published 返回 NOT_PUBLISHED，@approved 返回 NOT_FOUND。
type Publications interface {
	// Published 返回资产当前有效的发布版本；未发布或已暂停时 NOT_PUBLISHED。
	Published(ctx context.Context, assetID ids.ID) (ids.ID, error)
	// Approved 返回审定通过且可用的号码最大的版本；没有时 NOT_FOUND。
	Approved(ctx context.Context, assetID ids.ID) (ids.ID, error)
}

// Ledger 是 catalog 使用的台账接口集合。
type Ledger interface {
	commit.Ledger
	commit.Reader
	commit.Namespace
	commit.Metadata
	commit.Projects
}

// Deps 是 catalog 依赖的实例资源与其他模块接口。
type Deps struct {
	// Home 是数据根目录（绝对路径）；Gate 是实例写入口，文件写入同样经它进入。
	Home string
	Gate *commands.Gate
	// Storage 是字节端；Ledger 是台账（提交、读取、占名、说明修订与项目登记）。
	Storage *storage.Service
	Ledger  Ledger
	Authz   authz.Authorizer
	Rights  rights.Evaluator
	// Publications 可选（M2）。
	Publications Publications
	Clock        clock.Clock
	IDs          *ids.Generator
	InstanceID   ids.ID
	// FS 可注入文件系统故障，只供测试使用。
	FS fileop.FS
}

// Service 是 catalog 的应用服务。
type Service struct {
	home     string
	layout   storage.Layout
	gate     *commands.Gate
	storage  *storage.Service
	ledger   Ledger
	authz    authz.Authorizer
	rights   rights.Evaluator
	pubs     Publications
	clock    clock.Clock
	ids      *ids.Generator
	instance ids.ID
	fs       fileop.FS
}

var _ commit.RevisionVerifier = (*Service)(nil)

// New 创建服务。
func New(d Deps) (*Service, error) {
	switch {
	case d.Storage == nil || d.Ledger == nil || d.Gate == nil:
		return nil, errors.New("catalog: storage, ledger and gate are required")
	case d.Authz == nil || d.Rights == nil || d.IDs == nil:
		return nil, errors.New("catalog: authorizer, rights evaluator and id generator are required")
	case !d.InstanceID.Valid():
		return nil, errors.New("catalog: instance id is required")
	case !filepath.IsAbs(d.Home):
		return nil, fmt.Errorf("catalog: data root %q must be an absolute path", d.Home)
	}
	if d.Clock == nil {
		d.Clock = clock.System{}
	}
	home := filepath.Clean(d.Home)
	return &Service{
		home: home, layout: storage.Layout{Home: home}, gate: d.Gate, storage: d.Storage, ledger: d.Ledger,
		authz: d.Authz, rights: d.Rights, pubs: d.Publications, clock: d.Clock, ids: d.IDs, instance: d.InstanceID, fs: d.FS,
	}, nil
}

func (s *Service) now() time.Time { return clock.Truncate(s.clock.Now()) }

// authorize 按当前权威状态判定，拒绝转换为结构化错误。
func (s *Service) authorize(ctx context.Context, who authz.Context, action authz.Action, res authz.Resource) error {
	d, err := s.authz.Authorize(ctx, who, action, res)
	if err != nil {
		return err
	}
	return d.Err()
}

// canRead 核对调用者可以读取项目内的对象；无权时统一 NOT_FOUND，不泄露存在性。
func (s *Service) canRead(ctx context.Context, who authz.Context, project ids.ID, kind string, id ids.ID) error {
	d, err := s.authz.Authorize(ctx, who, ActionRead, authz.Resource{ProjectID: project, Kind: kind, ID: id})
	if err != nil {
		return err
	}
	if d.Allowed {
		return nil
	}
	switch d.Code {
	case errcode.TokenExpired, errcode.TokenRevoked, errcode.AuthRequired:
		return d.Err()
	}
	return errcode.New(errcode.NotFound, "")
}

// write 经实例写入口取得屏障共享锁；catalog 的文件写入与业务写入一样服从维护屏障。
func (s *Service) write(ctx context.Context) (context.Context, func(), error) {
	lctx, h, err := s.gate.Acquire(ctx, commands.Request{})
	if err != nil {
		return ctx, nil, err
	}
	return lctx, h.Release, nil
}

// ref 把数据根下的路径转为内部引用（小写、/ 分隔，不是主机路径）。
func (s *Service) ref(path string) string {
	rel, err := filepath.Rel(s.home, path)
	if err != nil {
		return ""
	}
	return strings.ToLower(filepath.ToSlash(rel))
}

func invalid(format string, args ...any) *errcode.Error {
	return errcode.Newf(errcode.SchemaInvalid, format, args...)
}

func reasonErr(code errcode.Code, reason, message string) *errcode.Error {
	return errcode.New(code, message).WithDetails(errcode.Detail{Reason: reason, Message: message})
}
