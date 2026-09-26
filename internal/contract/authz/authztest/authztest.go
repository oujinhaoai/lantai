// Package authztest 提供 authz 接口的内存桩，供其他模块在 T01 完成前测试
// 授权、撤权与恢复代次相关行为。它只模拟契约语义，不是安全实现。
package authztest

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// Static 是可编程的授权桩：按 (主体, 项目) 授予动作，撤权立即影响之后的判定。
type Static struct {
	mu            sync.Mutex
	clock         clock.Clock
	instance      ids.ID
	recoveryEpoch int64
	policyRev     int64
	principals    map[ids.ID]*principal
	sessions      map[ids.ID]*authz.Context
	ended         map[ids.ID]bool
}

type principal struct {
	kind      authz.PrincipalKind
	authEpoch int64
	grants    map[ids.ID][]authz.Action // 项目 → 动作；实例级对象用空 ID
}

var _ authz.Authorizer = (*Static)(nil)
var _ authz.SessionVerifier = (*Static)(nil)
var _ authz.EpochSource = (*Static)(nil)

// New 创建授权桩，恢复代次与策略修订从 1 开始。
func New(clk clock.Clock, instance ids.ID) *Static {
	return &Static{
		clock: clk, instance: instance, recoveryEpoch: 1, policyRev: 1,
		principals: map[ids.ID]*principal{}, sessions: map[ids.ID]*authz.Context{}, ended: map[ids.ID]bool{},
	}
}

// AddPrincipal 登记主体。
func (s *Static) AddPrincipal(id ids.ID, kind authz.PrincipalKind) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.principals[id] = &principal{kind: kind, authEpoch: 1, grants: map[ids.ID][]authz.Action{}}
}

// Grant 授予主体在项目内的动作。
func (s *Static) Grant(p ids.ID, project ids.ID, actions ...authz.Action) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pr := s.principals[p]
	pr.grants[project] = append(pr.grants[project], actions...)
	s.policyRev++
}

// Revoke 撤销主体在项目内的全部动作，推进策略修订与 auth_epoch；
// 返回后新的授权判定一律拒绝。
func (s *Static) Revoke(p ids.ID, project ids.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pr := s.principals[p]
	delete(pr.grants, project)
	pr.authEpoch++
	s.policyRev++
}

// OpenSession 为主体开启会话，ttl 后过期。
func (s *Static) OpenSession(p ids.ID, ttl time.Duration, projects ...ids.ID) authz.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	pr := s.principals[p]
	c := authz.Context{
		InstanceID: s.instance, PrincipalID: p, PrincipalKind: pr.kind, SessionID: ids.New(),
		AuthEpoch: pr.authEpoch, RecoveryEpoch: s.recoveryEpoch, PolicyRevision: s.policyRev,
		Projects: slices.Clone(projects), ExpiresAt: s.clock.Now().Add(ttl),
	}
	s.sessions[c.SessionID] = &c
	return c
}

// EndSession 结束会话。
func (s *Static) EndSession(id ids.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ended[id] = true
}

// Restore 模拟从备份恢复：推进恢复代次，全部旧会话失效。
func (s *Static) Restore() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recoveryEpoch++
	return s.recoveryEpoch
}

// RecoveryEpoch 实现 authz.EpochSource。
func (s *Static) RecoveryEpoch(context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recoveryEpoch, nil
}

// VerifySession 实现 authz.SessionVerifier。
func (s *Static) VerifySession(_ context.Context, id ids.ID) (authz.Context, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.sessions[id]
	if !ok {
		return authz.Context{}, errcode.New(errcode.AuthRequired, "")
	}
	if code := s.sessionProblem(*c); code != "" {
		return authz.Context{}, errcode.New(code, "")
	}
	out := *c
	out.PolicyRevision = s.policyRev
	return out, nil
}

func (s *Static) sessionProblem(c authz.Context) errcode.Code {
	pr, ok := s.principals[c.PrincipalID]
	switch {
	case !ok || s.ended[c.SessionID]:
		return errcode.TokenRevoked
	case c.RecoveryEpoch != s.recoveryEpoch || c.AuthEpoch != pr.authEpoch:
		return errcode.TokenRevoked
	case !s.clock.Now().Before(c.ExpiresAt):
		return errcode.TokenExpired
	}
	return ""
}

// Authorize 实现 authz.Authorizer：每次都按当前状态判定。
func (s *Static) Authorize(_ context.Context, who authz.Context, action authz.Action, res authz.Resource) (authz.Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := authz.Decision{PolicyRevision: s.policyRev}
	if code := s.sessionProblem(who); code != "" {
		d.Code = code
		return d, nil
	}
	pr := s.principals[who.PrincipalID]
	d.AuthEpoch = pr.authEpoch
	if !who.InScope(res.ProjectID) || !slices.Contains(pr.grants[res.ProjectID], action) {
		d.Code = errcode.Forbidden
		return d, nil
	}
	d.Allowed = true
	return d, nil
}
