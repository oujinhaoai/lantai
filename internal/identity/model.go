package identity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// namePatterns 按主体类别约束名称，名称在本馆唯一且注册后不改。
// 人：ada；Agent 身份：<Agent 类型>@<节点>；节点：node:<节点>；
// 执行器、运行器：worker:<名称>@<节点>、runner:<名称>@<节点>；服务：service:<名称>。
var namePatterns = map[authz.PrincipalKind]*regexp.Regexp{
	authz.Human:   regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`),
	authz.Agent:   regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}@[a-z0-9][a-z0-9-]{0,62}$`),
	authz.Node:    regexp.MustCompile(`^node:[a-z0-9][a-z0-9-]{0,62}$`),
	authz.Worker:  regexp.MustCompile(`^worker:[a-z][a-z0-9-]{0,31}@[a-z0-9][a-z0-9-]{0,62}$`),
	authz.Runner:  regexp.MustCompile(`^runner:[a-z][a-z0-9-]{0,31}@[a-z0-9][a-z0-9-]{0,62}$`),
	authz.Service: regexp.MustCompile(`^service:[a-z][a-z0-9-]{0,62}$`),
}

// ValidName 报告 name 是否符合该类别的命名规则。
func ValidName(kind authz.PrincipalKind, name string) bool {
	re, ok := namePatterns[kind]
	return ok && re.MatchString(name)
}

// PrincipalState 是主体状态；停用不删除，历史记录中的身份保留可查。
type PrincipalState string

const (
	StateActive   PrincipalState = "active"
	StateDisabled PrincipalState = "disabled"
)

// Profile 是主体档案。能力标签只用于匹配任务与调度，与角色分开：任何授权
// 判定都不读取能力标签；Agent 不能给自己加能力。
type Profile struct {
	AgentType    string   `json:"agent_type,omitempty"`
	Node         string   `json:"node,omitempty"`
	DefaultModel string   `json:"default_model,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	// MaxSessions 为同时有效的会话数上限，0 表示不限。
	MaxSessions int `json:"max_sessions,omitempty"`
}

var (
	capabilityRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	shortTextRE  = regexp.MustCompile(`^[\p{L}\p{N} ._:@/()+-]{0,128}$`)
)

func (p Profile) normalized() (Profile, error) {
	caps := slices.Clone(p.Capabilities)
	sort.Strings(caps)
	caps = slices.Compact(caps)
	if len(caps) > 64 {
		return p, fmt.Errorf("at most 64 capabilities")
	}
	for _, c := range caps {
		if !capabilityRE.MatchString(c) {
			return p, fmt.Errorf("capability %q must match %s", c, capabilityRE)
		}
	}
	for _, v := range []string{p.AgentType, p.Node, p.DefaultModel} {
		if !shortTextRE.MatchString(v) {
			return p, fmt.Errorf("profile field %q contains unsupported characters", v)
		}
	}
	if p.MaxSessions < 0 || p.MaxSessions > 1000 {
		return p, fmt.Errorf("max_sessions must be between 0 and 1000")
	}
	if len(caps) == 0 {
		caps = nil
	}
	p.Capabilities = caps
	return p, nil
}

// Principal 是登记的主体。
type Principal struct {
	ID          ids.ID              `json:"principal_id"`
	Kind        authz.PrincipalKind `json:"kind"`
	Name        string              `json:"name"`
	DisplayName string              `json:"display_name,omitempty"`
	State       PrincipalState      `json:"state"`
	AuthEpoch   int64               `json:"auth_epoch"`
	Profile     Profile             `json:"profile"`
	Revision    int64               `json:"revision"`
	CreatedAt   time.Time           `json:"created_at"`
	UpdatedAt   time.Time           `json:"updated_at"`
}

// Role 是项目角色。
type Role string

const (
	RoleOwner       Role = "owner"
	RoleCoordinator Role = "coordinator"
	RoleContributor Role = "contributor"
	RoleCurator     Role = "curator"
	RoleChecker     Role = "checker"
	RoleReviewer    Role = "reviewer"
	RoleViewer      Role = "viewer"
)

// AllRoles 是全部项目角色。
var AllRoles = []Role{RoleOwner, RoleCoordinator, RoleContributor, RoleCurator, RoleChecker, RoleReviewer, RoleViewer}

// Valid 报告角色是否已知。
func (r Role) Valid() bool { return slices.Contains(AllRoles, r) }

// SystemRole 是不属于项目的系统角色。
type SystemRole string

// RoleAdmin 是系统管理员：身份与凭据、全局策略等，只授予人。
const RoleAdmin SystemRole = "admin"

// Scope 是会话与长期凭据可使用的动作范围；它只收窄，不授予角色之外的权限。
type Scope string

const (
	ScopeRead     Scope = "read"
	ScopeIngest   Scope = "ingest"
	ScopeOrganize Scope = "organize"
	ScopeTask     Scope = "task"
	ScopeNode     Scope = "node"
	ScopeWorker   Scope = "worker"
	ScopeRunner   Scope = "runner"
	ScopeService  Scope = "service"
	// ScopeAdmin 只出现在人的会话中，用于身份、凭据与策略管理。
	ScopeAdmin Scope = "admin"
	// ScopeSelf 允许查看自己、结束自己的会话；每个普通会话都有。
	ScopeSelf Scope = "self"
	// ScopeRecovery 只出现在受限恢复/设置会话中。
	ScopeRecovery Scope = "recovery"
)

// kindScopes 规定每类主体的会话最多能有哪些范围；节点、执行器、运行器的
// 范围互不通用，Agent 与服务永远拿不到 admin。
var kindScopes = map[authz.PrincipalKind][]Scope{
	authz.Human:   {ScopeRead, ScopeIngest, ScopeOrganize, ScopeTask, ScopeAdmin, ScopeSelf},
	authz.Agent:   {ScopeRead, ScopeIngest, ScopeOrganize, ScopeTask, ScopeSelf},
	authz.Node:    {ScopeNode, ScopeSelf},
	authz.Worker:  {ScopeWorker, ScopeSelf},
	authz.Runner:  {ScopeRunner, ScopeSelf},
	authz.Service: {ScopeService, ScopeRead, ScopeSelf},
}

// AllowedScopes 返回某类主体可用的全部范围。
func AllowedScopes(kind authz.PrincipalKind) []Scope { return slices.Clone(kindScopes[kind]) }

// normalizeScopes 排序去重并检查 scopes 是否都在 allowed 内；self 总是加入。
func normalizeScopes(scopes, allowed []Scope) ([]Scope, error) {
	out := slices.Clone(scopes)
	if !slices.Contains(out, ScopeSelf) && slices.Contains(allowed, ScopeSelf) {
		out = append(out, ScopeSelf)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	out = slices.Compact(out)
	for _, sc := range out {
		if !slices.Contains(allowed, sc) {
			return nil, fmt.Errorf("scope %q is not available here", sc)
		}
	}
	return out, nil
}

func normalizeProjects(projects []ids.ID) ([]ids.ID, error) {
	out := slices.Clone(projects)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	out = slices.Compact(out)
	if len(out) > 256 {
		return nil, fmt.Errorf("at most 256 projects")
	}
	for _, p := range out {
		if !p.Valid() {
			return nil, fmt.Errorf("project %q is not a valid id", p)
		}
	}
	return out, nil
}

// subset 报告 narrow 是否在 wide 之内；wide 为空表示不限制。
func subset[T comparable](narrow, wide []T) bool {
	if len(wide) == 0 {
		return true
	}
	for _, v := range narrow {
		if !slices.Contains(wide, v) {
			return false
		}
	}
	return true
}

func jsonText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func decodeList[T any](s string) ([]T, error) {
	var out []T
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// jsonDecodeStrict 解码库中保存的 JSON，拒绝未知字段。
func jsonDecodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
