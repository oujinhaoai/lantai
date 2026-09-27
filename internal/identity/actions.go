package identity

import (
	"slices"
	"sort"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
)

// Level 区分实例级动作与项目内动作。
type Level int

const (
	// InstanceLevel 的资源没有 project_id，由系统角色或“本人”规则授权。
	InstanceLevel Level = iota
	// ProjectLevel 的资源属于项目，由主体在该项目中的角色授权。
	ProjectLevel
)

// ActionSpec 是一个可授权动作的规则。动作登记是权限矩阵的唯一来源；
// 未登记的动作一律拒绝。
type ActionSpec struct {
	Action authz.Action
	// Scope 是会话必须包含的范围。
	Scope Scope
	Level Level
	// Roles 是允许的项目角色（ProjectLevel）。
	Roles []Role
	// AdminAlso 表示系统管理员在任何项目也可执行（例如为新项目指定负责人）。
	AdminAlso bool
	// SystemRoles 是实例级动作要求的系统角色；为空表示本人即可。
	SystemRoles []SystemRole
	// HumanOnly 要求人的非委托会话。
	HumanOnly bool
	// HumanGrant 表示执行时还要出示绑定动作、目标与请求摘要的 HumanGrant；
	// Authorize 对这类动作永远返回 HUMAN_PROOF_REQUIRED，只能经敏感命令执行。
	HumanGrant bool
	// RecoveryOnly 表示只有受限恢复/设置会话可执行；其他动作一律拒绝恢复会话。
	RecoveryOnly bool
	// AllowRecovery 表示普通会话与恢复会话都可执行（只用于查看自己）。
	AllowRecovery bool
}

var allProjectRoles = AllRoles

// 身份与安全动作。
const (
	ActWhoAmI              authz.Action = "identity.whoami"
	ActEndSession          authz.Action = "identity.end_session"
	ActNarrowSession       authz.Action = "identity.narrow_session"
	ActReadPrincipals      authz.Action = "identity.read_principals"
	ActRegisterPrincipal   authz.Action = "identity.register_principal"
	ActUpdatePrincipal     authz.Action = "identity.update_principal"
	ActDisablePrincipal    authz.Action = "identity.disable_principal"
	ActEnablePrincipal     authz.Action = "identity.enable_principal"
	ActIssueCredential     authz.Action = "identity.issue_credential"
	ActRevokeCredential    authz.Action = "identity.revoke_credential"
	ActGrantSystemRole     authz.Action = "identity.grant_system_role"
	ActRevokeSystemRole    authz.Action = "identity.revoke_system_role"
	ActSetSystemPolicy     authz.Action = "identity.set_system_policy"
	ActResetHumanFactor    authz.Action = "identity.reset_human_factor"
	ActRevokeSession       authz.Action = "identity.revoke_session"
	ActReadMembers         authz.Action = "identity.read_members"
	ActGrantProjectRole    authz.Action = "identity.grant_project_role"
	ActRevokeProjectRole   authz.Action = "identity.revoke_project_role"
	ActSetProjectPolicy    authz.Action = "identity.set_project_policy"
	ActRecoveryEnroll      authz.Action = "identity.recovery_enroll"
	ActRecoveryConfirm     authz.Action = "identity.recovery_confirm"
	ActRecoverySetPassword authz.Action = "identity.recovery_set_password"
)

// M1 其他模块在最终接受与下载检查时使用的动作。新增业务动作在这里登记，
// 由身份模块统一给出角色矩阵，不在各模块另写一套权限判断。
const (
	ActCatalogRead          authz.Action = "catalog.read"
	ActStorageReadContent   authz.Action = "storage.read_content"
	ActQuerySearch          authz.Action = "query.search"
	ActEventsRead           authz.Action = "events.read"
	ActStorageUpload        authz.Action = "storage.upload"
	ActCatalogCreateAsset   authz.Action = "catalog.create_asset"
	ActLedgerCommitVersion  authz.Action = "ledger.commit_version"
	ActCatalogPatchMetadata authz.Action = "catalog.patch_metadata"
	// ActCatalogPatchOwnMetadata 用于制作者修改自己提交的版本的著录；
	// “是否本人提交”由 catalog 核对后再选择这个动作。
	ActCatalogPatchOwnMetadata authz.Action = "catalog.patch_own_metadata"
	// ActCatalogCreateProject 登记新项目（稳定 ID 与不可变 key）；只有系统管理员本人。
	ActCatalogCreateProject authz.Action = "catalog.create_project"
	// ActCatalogPatchProject 修改项目说明（project.yaml）。
	ActCatalogPatchProject authz.Action = "catalog.patch_project"
)

var adminOnly = []SystemRole{RoleAdmin}

var actionList = []ActionSpec{
	{Action: ActWhoAmI, Scope: ScopeSelf, AllowRecovery: true},
	{Action: ActEndSession, Scope: ScopeSelf, AllowRecovery: true},
	{Action: ActNarrowSession, Scope: ScopeSelf},
	{Action: ActReadPrincipals, Scope: ScopeAdmin, SystemRoles: adminOnly, HumanOnly: true},
	{Action: ActRegisterPrincipal, Scope: ScopeAdmin, SystemRoles: adminOnly, HumanOnly: true, HumanGrant: true},
	{Action: ActUpdatePrincipal, Scope: ScopeAdmin, SystemRoles: adminOnly, HumanOnly: true, HumanGrant: true},
	{Action: ActDisablePrincipal, Scope: ScopeAdmin, SystemRoles: adminOnly, HumanOnly: true, HumanGrant: true},
	{Action: ActEnablePrincipal, Scope: ScopeAdmin, SystemRoles: adminOnly, HumanOnly: true, HumanGrant: true},
	{Action: ActIssueCredential, Scope: ScopeAdmin, SystemRoles: adminOnly, HumanOnly: true, HumanGrant: true},
	{Action: ActRevokeCredential, Scope: ScopeAdmin, SystemRoles: adminOnly, HumanOnly: true, HumanGrant: true},
	{Action: ActGrantSystemRole, Scope: ScopeAdmin, SystemRoles: adminOnly, HumanOnly: true, HumanGrant: true},
	{Action: ActRevokeSystemRole, Scope: ScopeAdmin, SystemRoles: adminOnly, HumanOnly: true, HumanGrant: true},
	{Action: ActSetSystemPolicy, Scope: ScopeAdmin, SystemRoles: adminOnly, HumanOnly: true, HumanGrant: true},
	{Action: ActResetHumanFactor, Scope: ScopeAdmin, SystemRoles: adminOnly, HumanOnly: true, HumanGrant: true},
	{Action: ActRevokeSession, Scope: ScopeAdmin, SystemRoles: adminOnly, HumanOnly: true, HumanGrant: true},

	{Action: ActReadMembers, Scope: ScopeRead, Level: ProjectLevel, Roles: allProjectRoles, AdminAlso: true},
	{Action: ActGrantProjectRole, Scope: ScopeAdmin, Level: ProjectLevel, Roles: []Role{RoleOwner}, AdminAlso: true, HumanOnly: true, HumanGrant: true},
	{Action: ActRevokeProjectRole, Scope: ScopeAdmin, Level: ProjectLevel, Roles: []Role{RoleOwner}, AdminAlso: true, HumanOnly: true, HumanGrant: true},
	{Action: ActSetProjectPolicy, Scope: ScopeAdmin, Level: ProjectLevel, Roles: []Role{RoleOwner}, AdminAlso: true, HumanOnly: true, HumanGrant: true},

	{Action: ActRecoveryEnroll, Scope: ScopeRecovery, RecoveryOnly: true},
	{Action: ActRecoveryConfirm, Scope: ScopeRecovery, RecoveryOnly: true},
	{Action: ActRecoverySetPassword, Scope: ScopeRecovery, RecoveryOnly: true},

	{Action: ActCatalogRead, Scope: ScopeRead, Level: ProjectLevel, Roles: allProjectRoles},
	{Action: ActStorageReadContent, Scope: ScopeRead, Level: ProjectLevel, Roles: allProjectRoles},
	{Action: ActQuerySearch, Scope: ScopeRead, Level: ProjectLevel, Roles: allProjectRoles},
	{Action: ActEventsRead, Scope: ScopeRead, Level: ProjectLevel, Roles: allProjectRoles},
	{Action: ActStorageUpload, Scope: ScopeIngest, Level: ProjectLevel, Roles: []Role{RoleOwner, RoleContributor}},
	{Action: ActCatalogCreateAsset, Scope: ScopeIngest, Level: ProjectLevel, Roles: []Role{RoleOwner, RoleContributor}},
	{Action: ActLedgerCommitVersion, Scope: ScopeIngest, Level: ProjectLevel, Roles: []Role{RoleOwner, RoleContributor}},
	{Action: ActCatalogPatchMetadata, Scope: ScopeOrganize, Level: ProjectLevel, Roles: []Role{RoleOwner, RoleCurator}},
	{Action: ActCatalogPatchOwnMetadata, Scope: ScopeOrganize, Level: ProjectLevel, Roles: []Role{RoleOwner, RoleContributor, RoleCurator}},
	{Action: ActCatalogCreateProject, Scope: ScopeAdmin, SystemRoles: adminOnly, HumanOnly: true},
	{Action: ActCatalogPatchProject, Scope: ScopeOrganize, Level: ProjectLevel, Roles: []Role{RoleOwner}, AdminAlso: true},
}

var actions = func() map[authz.Action]ActionSpec {
	m := make(map[authz.Action]ActionSpec, len(actionList))
	for _, a := range actionList {
		if !a.Action.Valid() {
			panic("identity: invalid action name " + string(a.Action))
		}
		if _, dup := m[a.Action]; dup {
			panic("identity: duplicate action " + string(a.Action))
		}
		m[a.Action] = a
	}
	return m
}()

// Spec 返回动作的规则；未登记返回 false。
func Spec(a authz.Action) (ActionSpec, bool) {
	s, ok := actions[a]
	return s, ok
}

// Actions 返回按名称排序的全部动作规则。
func Actions() []ActionSpec {
	out := slices.Clone(actionList)
	sort.Slice(out, func(i, j int) bool { return out[i].Action < out[j].Action })
	return out
}
