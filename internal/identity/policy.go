package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

type policyKind int

const (
	policyBool policyKind = iota
	policyInt
	policyEnum
	policyList
)

// PolicySpec 是一项策略的类型、默认值与取值范围。策略分三级（系统默认 →
// 实例覆盖 → 项目覆盖；资产类型覆盖随 M2 领域规则加入），全部在服务端执行。
type PolicySpec struct {
	Key      string
	kind     policyKind
	Default  any
	min, max int64
	enum     []string
	// Fixed 表示值固定为默认值，任何覆盖都拒绝（例如破坏性操作必须由人执行、
	// D18 回收站参数）。
	Fixed bool
}

var listItemRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/@-]{0,127}$`)

var policyList_ = []PolicySpec{
	{Key: "review.require_human", kind: policyBool, Default: true},
	{Key: "review.allow_self", kind: policyBool, Default: false},
	{Key: "review.allow_self_human", kind: policyBool, Default: true},
	{Key: "review.qa_required", kind: policyBool, Default: false},
	{Key: "review.auto_approve", kind: policyBool, Default: false},
	{Key: "review.withdraw_superseded", kind: policyBool, Default: true},
	{Key: "ingest.final_only", kind: policyBool, Default: true},
	{Key: "task.lease_minutes", kind: policyInt, Default: int64(30), min: 1, max: 1440},
	{Key: "task.max_attempts", kind: policyInt, Default: int64(3), min: 1, max: 100},
	{Key: "task.rework_hold_hours", kind: policyInt, Default: int64(24), min: 0, max: 720},
	{Key: "visibility", kind: policyEnum, Default: "normal", enum: []string{"normal", "restricted"}},
	{Key: "usage.default", kind: policyEnum, Default: "production",
		enum: []string{"archive_review", "reference", "production", "generative_input", "raw_export"}},
	{Key: "runner.playbooks", kind: policyList, Default: []string{}},
	{Key: "destructive.require_human", kind: policyBool, Default: true, Fixed: true},
	{Key: "trash.retention_days", kind: policyInt, Default: int64(30), min: 7, max: 365},
	{Key: "trash.auto_purge", kind: policyBool, Default: true},
	{Key: "trash.agent_scope", kind: policyEnum, Default: "own_unapproved", enum: []string{"own_unapproved"}, Fixed: true},
	{Key: "trash.agent_hourly_limit", kind: policyInt, Default: int64(20), Fixed: true},
	{Key: "trash.bulk_confirm", kind: policyInt, Default: int64(50), Fixed: true},
	{Key: "trash.fresh_hours", kind: policyInt, Default: int64(3), Fixed: true},
	{Key: "trash.fresh_retention_days", kind: policyInt, Default: int64(7), Fixed: true},
	{Key: "trash.fresh_hourly_limit", kind: policyInt, Default: int64(200), Fixed: true},
	{Key: "publish.mode", kind: policyEnum, Default: "auto", enum: []string{"auto", "manual"}},
	{Key: "lock.by_flow", kind: policyBool, Default: false},
	{Key: "plugins.allowed", kind: policyList, Default: []string{}},
	{Key: "plugins.breaker_failures", kind: policyInt, Default: int64(5), min: 1, max: 1000},
	{Key: "plugins.breaker_window_seconds", kind: policyInt, Default: int64(600), min: 10, max: 86400},
	{Key: "plugins.breaker_cooldown_seconds", kind: policyInt, Default: int64(900), min: 10, max: 86400},
	{Key: "plugins.breaker_half_open_max_calls", kind: policyInt, Default: int64(1), min: 1, max: 10},
}

var policies = func() map[string]PolicySpec {
	m := map[string]PolicySpec{}
	for _, p := range policyList_ {
		m[p.Key] = p
	}
	return m
}()

// PolicyKeys 返回已登记的策略键。
func PolicyKeys() []string {
	out := make([]string, 0, len(policies))
	for k := range policies {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// normalizePolicy 校验并规范化策略值，返回 RFC 8785 JSON。
func normalizePolicy(key string, raw json.RawMessage) (string, error) {
	spec, ok := policies[key]
	if !ok {
		return "", fmt.Errorf("unknown policy %q", key)
	}
	doc, err := canonjson.Decode(raw)
	if err != nil {
		return "", fmt.Errorf("policy %s: %v", key, err)
	}
	switch spec.kind {
	case policyBool:
		if _, ok := doc.(bool); !ok {
			return "", fmt.Errorf("policy %s must be true or false", key)
		}
	case policyInt:
		n, ok := doc.(json.Number)
		if !ok {
			return "", fmt.Errorf("policy %s must be an integer", key)
		}
		v, err := n.Int64()
		if err != nil {
			return "", fmt.Errorf("policy %s must be an integer", key)
		}
		if !spec.Fixed && (v < spec.min || v > spec.max) {
			return "", fmt.Errorf("policy %s must be between %d and %d", key, spec.min, spec.max)
		}
	case policyEnum:
		v, ok := doc.(string)
		if !ok || !slices.Contains(spec.enum, v) {
			return "", fmt.Errorf("policy %s must be one of %v", key, spec.enum)
		}
	case policyList:
		items, ok := doc.([]any)
		if !ok || len(items) > 256 {
			return "", fmt.Errorf("policy %s must be a list of at most 256 identifiers", key)
		}
		for _, it := range items {
			v, ok := it.(string)
			if !ok || !listItemRE.MatchString(v) {
				return "", fmt.Errorf("policy %s items must be identifiers", key)
			}
		}
	}
	canon, err := canonjson.Canonicalize(raw)
	if err != nil {
		return "", err
	}
	if spec.Fixed {
		def, _ := canonjson.CanonicalizeValue(spec.Default)
		if string(def) != string(canon) {
			return "", fmt.Errorf("policy %s is fixed and cannot be overridden", key)
		}
	}
	return string(canon), nil
}

// PolicyValue 是一项生效策略及其来源。
type PolicyValue struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
	// Source 为 default、instance 或 project。
	Source   string `json:"source"`
	Revision int64  `json:"revision,omitempty"`
}

// ResolvePolicies 返回项目（为空时为实例）的生效策略，供服务端模块读取；
// 它不做调用者授权，面向客户端时用 Policies。
func (s *Service) ResolvePolicies(ctx context.Context, project ids.ID) (map[string]PolicyValue, error) {
	return resolvePolicies(ctx, s.main, project)
}

func resolvePolicies(ctx context.Context, q commands.DBTX, project ids.ID) (map[string]PolicyValue, error) {
	out := map[string]PolicyValue{}
	for _, spec := range policyList_ {
		def, err := canonjson.CanonicalizeValue(spec.Default)
		if err != nil {
			return nil, err
		}
		out[spec.Key] = PolicyValue{Key: spec.Key, Value: def, Source: "default"}
	}
	scopes := []string{""}
	if project != "" {
		scopes = append(scopes, string(project))
	}
	for _, scope := range scopes {
		rows, err := q.QueryContext(ctx, `SELECT key, value, revision FROM identity_policies WHERE scope = ?`, scope)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var pv PolicyValue
			var value string
			if err := rows.Scan(&pv.Key, &value, &pv.Revision); err != nil {
				rows.Close()
				return nil, err
			}
			if _, known := policies[pv.Key]; !known {
				continue
			}
			pv.Value = json.RawMessage(value)
			pv.Source = "instance"
			if scope != "" {
				pv.Source = "project"
			}
			out[pv.Key] = pv
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Policies 返回调用者可见的生效策略：项目策略要求该项目的成员资格（或管理员），
// 实例策略要求普通会话。
func (s *Service) Policies(ctx context.Context, who authz.Context, project ids.ID) (map[string]PolicyValue, error) {
	v, err := s.current(ctx, who)
	if err != nil {
		return nil, err
	}
	if project != "" {
		if err := s.require(ctx, s.main, v, ActReadMembers, authz.Resource{ProjectID: project}); err != nil {
			return nil, err
		}
	} else if v.sess.Kind != sessionNormal && v.sess.Kind != sessionDelegated {
		return nil, errcode.New(errcode.Forbidden, "")
	}
	return resolvePolicies(ctx, s.main, project)
}

// PolicyInfo 是一项策略登记的公开描述，供文档与客户端提示使用。
type PolicyInfo struct {
	Key string
	// Default 是默认值的 RFC 8785 JSON。
	Default string
	// Allowed 描述允许的取值。
	Allowed string
	// Fixed 表示不允许覆盖。
	Fixed bool
}

// PolicyCatalog 返回按键排序的策略登记。
func PolicyCatalog() []PolicyInfo {
	out := make([]PolicyInfo, 0, len(policyList_))
	for _, p := range policyList_ {
		def, _ := canonjson.CanonicalizeValue(p.Default)
		info := PolicyInfo{Key: p.Key, Default: string(def), Fixed: p.Fixed}
		switch {
		case p.Fixed:
			info.Allowed = "fixed"
		case p.kind == policyBool:
			info.Allowed = "true / false"
		case p.kind == policyInt:
			info.Allowed = fmt.Sprintf("%d–%d", p.min, p.max)
		case p.kind == policyEnum:
			info.Allowed = strings.Join(p.enum, " / ")
		case p.kind == policyList:
			info.Allowed = "identifier list (≤256)"
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
