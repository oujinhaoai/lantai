package identity

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// decide 按权限矩阵判定已核对会话能否对资源执行动作；返回空表示允许。
// q 可以是主库连接或主库事务（敏感命令在同一事务中判定与提交）。HumanGrant
// 类动作在这里只判定“是否有资格”，是否出示了有效授权由敏感命令另行核对。
func (s *Service) decide(ctx context.Context, q commands.DBTX, v verified, spec ActionSpec, res authz.Resource) (errcode.Code, error) {
	sess, p := v.sess, v.principal
	switch {
	case spec.RecoveryOnly:
		if sess.Kind != sessionRecovery {
			return errcode.Forbidden, nil
		}
	case sess.Kind == sessionRecovery && !spec.AllowRecovery:
		// 受限恢复会话不能取得资源访问、敏感操作或任何普通权限。
		return errcode.Forbidden, nil
	}
	if !slices.Contains(sess.Scopes, spec.Scope) {
		return errcode.Forbidden, nil
	}
	// 委托会话只能执行所列动作；查看自己、结束自己不受此限。
	if len(sess.Actions) > 0 && spec.Scope != ScopeSelf && !slices.Contains(sess.Actions, spec.Action) {
		return errcode.Forbidden, nil
	}
	if spec.HumanOnly && !interactiveHuman(v) {
		return errcode.Forbidden, nil
	}
	switch spec.Level {
	case InstanceLevel:
		if res.ProjectID != "" {
			return errcode.Forbidden, nil
		}
		// 收窄到项目的会话只能管理自己，不能执行实例级管理。
		if len(sess.Projects) > 0 && spec.Scope != ScopeSelf && spec.Scope != ScopeRecovery {
			return errcode.Forbidden, nil
		}
		for _, role := range spec.SystemRoles {
			ok, err := hasSystemRole(ctx, q, p.ID, role)
			if err != nil {
				return "", err
			}
			if !ok {
				return errcode.Forbidden, nil
			}
		}
	case ProjectLevel:
		if !res.ProjectID.Valid() {
			return errcode.Forbidden, nil
		}
		// 会话收窄范围之外的项目不泄露存在性。
		if len(sess.Projects) > 0 && !slices.Contains(sess.Projects, res.ProjectID) {
			return errcode.NotFound, nil
		}
		roles, err := projectRolesOf(ctx, q, res.ProjectID, p.ID)
		if err != nil {
			return "", err
		}
		admin := false
		if spec.AdminAlso && interactiveHuman(v) {
			admin, err = hasSystemRole(ctx, q, p.ID, RoleAdmin)
			if err != nil {
				return "", err
			}
		}
		if !admin {
			if len(roles) == 0 {
				return errcode.NotFound, nil
			}
			if !slices.ContainsFunc(roles, func(r Role) bool { return slices.Contains(spec.Roles, r) }) {
				return errcode.Forbidden, nil
			}
		}
	}
	if spec.Action == ActPersonalRead {
		policies, err := resolvePolicies(ctx, q, res.ProjectID)
		if err != nil {
			return "", err
		}
		policy := policies["personal.readers"]
		var readers []ids.ID
		if policy.Source != "project" || json.Unmarshal(policy.Value, &readers) != nil || !slices.Contains(readers, p.ID) {
			return errcode.Forbidden, nil
		}
	}
	return "", nil
}

// Authorize 实现 authz.Authorizer：每次都读取当前权威状态——会话是否仍有效、
// auth_epoch 与 recovery_epoch 是否仍是当前值、角色与收窄范围——不使用缓存。
// 需要人类授权的动作一律返回 HUMAN_PROOF_REQUIRED：它们只能经 Execute 在
// 出示绑定的 HumanGrant 后执行。未登记的动作拒绝。
func (s *Service) Authorize(ctx context.Context, who authz.Context, action authz.Action, res authz.Resource) (authz.Decision, error) {
	d := authz.Decision{}
	spec, ok := Spec(action)
	if !ok {
		d.Code = errcode.Forbidden
		return d, nil
	}
	v, err := s.verify(ctx, who.SessionID)
	if err != nil {
		return d, err
	}
	if v.problem != "" {
		d.Code = v.problem
		return d, nil
	}
	if v.principal.ID != who.PrincipalID {
		d.Code = errcode.TokenRevoked
		return d, nil
	}
	d.PolicyRevision, d.AuthEpoch = v.policyRev, v.principal.AuthEpoch
	code, err := s.decide(ctx, s.main, v, spec, res)
	if err != nil {
		return d, err
	}
	if code != "" {
		d.Code = code
		return d, nil
	}
	if spec.HumanGrant {
		d.Code = errcode.HumanProofRequired
		return d, nil
	}
	d.Allowed = true
	return d, nil
}

// Accept 是 T02/T03 等模块的最终接受边界：经维护屏障取 security_guard 读锁
// 与 locks 中的资源锁（顺序由协调器保证），在锁内按当前权威状态授权；允许时
// 调用 commit，由它完成所属业务库的提交。撤权、角色与策略变更持
// security_guard 写锁，因此撤权返回成功之后开始的 Accept 必然看到新状态；
// 已在锁内提交的结果保留。commit 内不得做上传或远程网络调用。
func (s *Service) Accept(ctx context.Context, who authz.Context, action authz.Action, res authz.Resource,
	locks commands.Request, commit func(ctx context.Context, d authz.Decision) error) error {
	locks.Barrier = commands.ModeShared
	locks.Security = commands.ModeShared
	lctx, h, err := s.gate.Acquire(ctx, locks)
	if err != nil {
		return err
	}
	defer h.Release()
	d, err := s.Authorize(lctx, who, action, res)
	if err != nil {
		return err
	}
	if !d.Allowed {
		return d.Err()
	}
	return commit(lctx, d)
}

// BeginRead 是下载等读取开始前的最终检查：取 security_guard 读锁，在锁内按
// 与 Accept 相同的语义授权，允许时调用 open 打开响应（锁内只打开，不传输）。
// 它不经维护屏障，维护期间读取照常；撤权返回后开始的读取一律拒绝，已经开始
// 发送的响应可能完成，不宣称能收回已交付的字节。
func (s *Service) BeginRead(ctx context.Context, who authz.Context, action authz.Action, res authz.Resource,
	open func(ctx context.Context, d authz.Decision) error) error {
	lctx, h, err := s.gate.Coordinator().Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return err
	}
	defer h.Release()
	d, err := s.Authorize(lctx, who, action, res)
	if err != nil {
		return err
	}
	if !d.Allowed {
		return d.Err()
	}
	return open(lctx, d)
}

// require 与 decide 相同，但把拒绝转换为结构化错误。
func (s *Service) require(ctx context.Context, q commands.DBTX, v verified, action authz.Action, res authz.Resource) error {
	code, err := s.decide(ctx, q, v, mustSpec(action), res)
	if err != nil {
		return err
	}
	if code != "" {
		return errcode.New(code, "")
	}
	return nil
}

// interactiveHuman 报告会话是否是人本人登录得到的交互会话：委托会话、子会话
// 与恢复会话都不算，不能通过“仅人本人”的检查，也不能申请人类授权。
func interactiveHuman(v verified) bool {
	return v.principal.Kind == authz.Human && v.sess.Kind == sessionNormal && v.sess.DelegatedBy == "" && v.sess.ParentID == ""
}
