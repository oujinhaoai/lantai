// Package authz 定义授权与恢复上下文的跨模块接口，由 identity（T01）实现。
//
// 主体身份、会话、auth_epoch、recovery_epoch 与策略修订都来自服务端验证，
// 不接受客户端自报的身份类别或传输档位。最终接受边界（提交、下载开始）
// 必须调用 Authorizer 读取当前权威状态，不能只凭缓存或旧决定放行；撤权
// 成功返回后，新的接受边界必须拒绝。
package authz

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// PrincipalKind 是服务端登记的主体类别。
type PrincipalKind string

const (
	Human   PrincipalKind = "human"
	Agent   PrincipalKind = "agent"
	Node    PrincipalKind = "node"
	Worker  PrincipalKind = "worker"
	Runner  PrincipalKind = "runner"
	Service PrincipalKind = "service"
)

// Valid 报告 k 是否为已知类别。
func (k PrincipalKind) Valid() bool {
	switch k {
	case Human, Agent, Node, Worker, Runner, Service:
		return true
	}
	return false
}

// TransferClass 是传输面的档位，由会话类别推导。
type TransferClass string

const (
	Interactive TransferClass = "interactive"
	Batch       TransferClass = "batch"
)

// Context 是经过验证的调用者上下文。
type Context struct {
	InstanceID    ids.ID
	PrincipalID   ids.ID
	PrincipalKind PrincipalKind
	SessionID     ids.ID
	// DelegatedBy 记录代为调用的插件、runner 等身份；与被委托主体分别审计，
	// 不能借此冒充主体本人。
	DelegatedBy    ids.ID
	AuthEpoch      int64
	RecoveryEpoch  int64
	PolicyRevision int64
	// Projects 非空时把会话收窄到这些项目；空表示不额外收窄。
	Projects  []ids.ID
	ExpiresAt time.Time
}

// ErrInvalidContext 表示上下文不完整。
var ErrInvalidContext = errors.New("authz: invalid caller context")

// Validate 检查字段完整性。
func (c Context) Validate() error {
	switch {
	case !c.PrincipalID.Valid():
		return fmt.Errorf("%w: principal_id", ErrInvalidContext)
	case !c.PrincipalKind.Valid():
		return fmt.Errorf("%w: principal_kind %q", ErrInvalidContext, c.PrincipalKind)
	case !c.SessionID.Valid():
		return fmt.Errorf("%w: session_id", ErrInvalidContext)
	case c.AuthEpoch < 1 || c.RecoveryEpoch < 1:
		return fmt.Errorf("%w: epochs", ErrInvalidContext)
	case c.ExpiresAt.IsZero():
		return fmt.Errorf("%w: expires_at", ErrInvalidContext)
	}
	return nil
}

// TransferClass 由主体类别决定：只有人本人的会话是交互档；其余（含代人
// 调用的插件、adapter 等委托会话）一律批量档，客户端自报不能提升档位。
func (c Context) TransferClass() TransferClass {
	if c.PrincipalKind == Human && c.DelegatedBy == "" {
		return Interactive
	}
	return Batch
}

// InScope 报告会话收窄是否允许访问项目。
func (c Context) InScope(project ids.ID) bool {
	return len(c.Projects) == 0 || slices.Contains(c.Projects, project)
}

// Action 是被授权的动作，形如 <module>.<verb>，例如 ledger.commit_version。
type Action string

var actionRE = regexp.MustCompile(`^[a-z][a-z0-9_]*\.[a-z][a-z0-9_]*$`)

// Valid 报告动作格式是否正确。
func (a Action) Valid() bool { return actionRE.MatchString(string(a)) }

// Purpose 是声明的使用用途（许可与限制按用途判断）。
type Purpose string

const (
	PurposeArchiveReview   Purpose = "archive_review"
	PurposeReference       Purpose = "reference"
	PurposeProduction      Purpose = "production"
	PurposeGenerativeInput Purpose = "generative_input"
	PurposeRawExport       Purpose = "raw_export"
)

// Resource 是被访问的对象；ProjectID 为空表示实例级对象。
type Resource struct {
	ProjectID ids.ID
	Kind      string
	ID        ids.ID
}

// Decision 是一次授权判定。
type Decision struct {
	Allowed bool
	// Code 在拒绝时给出错误码：FORBIDDEN、HUMAN_PROOF_REQUIRED、USE_RESTRICTED，
	// 或为隐藏对象存在性而返回 NOT_FOUND；会话失效为 TOKEN_REVOKED/TOKEN_EXPIRED。
	Code errcode.Code
	// PolicyRevision 与 AuthEpoch 是判定所依据的权威修订，调用方可记入操作。
	PolicyRevision int64
	AuthEpoch      int64
}

// Err 把拒绝转换为结构化错误；允许时返回 nil。
func (d Decision) Err() error {
	if d.Allowed {
		return nil
	}
	code := d.Code
	if _, ok := errcode.Lookup(code); !ok {
		code = errcode.Forbidden
	}
	return errcode.New(code, "")
}

// Authorizer 判定调用者能否对资源执行动作。实现必须读取当前权威状态，
// 包括会话是否仍有效、auth_epoch 与 recovery_epoch 是否仍是当前值。
type Authorizer interface {
	Authorize(ctx context.Context, who Context, action Action, res Resource) (Decision, error)
}

// SessionVerifier 验证会话并返回可信上下文；旧代次或已结束的会话返回
// TOKEN_REVOKED，过期返回 TOKEN_EXPIRED。
type SessionVerifier interface {
	VerifySession(ctx context.Context, sessionID ids.ID) (Context, error)
}

// EpochSource 返回实例当前的恢复代次。每次从备份恢复都会递增，旧会话、
// 旧执行轮次与旧授权随之失效。
type EpochSource interface {
	RecoveryEpoch(ctx context.Context) (int64, error)
}
