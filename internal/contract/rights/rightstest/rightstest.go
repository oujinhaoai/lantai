// Package rightstest 提供 rights.Evaluator 的可编程内存桩，供 storage、catalog
// 在 provenance（T03.2）完成前测试下载与复用的限制分支。默认放行并不代表
// 真实实现也默认放行：真实实现在证据不足时必须返回 RIGHTS_PENDING。
package rightstest

import (
	"context"
	"sync"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/rights"
)

// Static 按版本与用途返回预设判定。
type Static struct {
	mu    sync.Mutex
	epoch int64
	rules map[key]errcode.Code
	calls int
}

type key struct {
	version ids.ID
	purpose authz.Purpose
}

var _ rights.Evaluator = (*Static)(nil)

// New 创建桩；未设置规则的版本与用途一律允许，rights_epoch 从 1 开始。
func New() *Static { return &Static{epoch: 1, rules: map[key]errcode.Code{}} }

// Restrict 让版本的某个用途（purpose 为空表示全部用途）返回 USE_RESTRICTED，并推进 rights_epoch。
func (s *Static) Restrict(version ids.ID, purpose authz.Purpose) {
	s.set(version, purpose, errcode.UseRestricted)
}

// Pending 让版本的某个用途返回 RIGHTS_PENDING，并推进 rights_epoch。
func (s *Static) Pending(version ids.ID, purpose authz.Purpose) {
	s.set(version, purpose, errcode.RightsPending)
}

// Clear 解除版本的全部规则，并推进 rights_epoch。
func (s *Static) Clear(version ids.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.rules {
		if k.version == version {
			delete(s.rules, k)
		}
	}
	s.epoch++
}

func (s *Static) set(version ids.ID, purpose authz.Purpose, code errcode.Code) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules[key{version, purpose}] = code
	s.epoch++
}

// Calls 返回判定次数，用于确认调用方逐请求核验而没有缓存。
func (s *Static) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// EvaluateUse 实现 rights.Evaluator。
func (s *Static) EvaluateUse(_ context.Context, _ authz.Context, ref ids.PermanentRef, purpose authz.Purpose) (rights.Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	d := rights.Decision{Allowed: true, RightsEpoch: s.epoch}
	for _, k := range []key{{ref.VersionID, purpose}, {ref.VersionID, ""}} {
		if code, ok := s.rules[k]; ok {
			d.Allowed, d.Code = false, code
			if code == errcode.RightsPending {
				d.RetryAfter = time.Second
			}
			break
		}
	}
	return d, nil
}
