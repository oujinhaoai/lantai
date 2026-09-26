// Package ownership 是模块、数据库表、文件区域与事件类型所有权的唯一登记。
//
// 规则：每张业务表只有一个模块写入，表名以所属模块名加下划线开头；模块只能
// 在登记允许的库里建表；跨模块、跨库不联表、不建外键、不共用 SQL 事务。
// 回执/operations/outbox 等基础设施表由共享组件写入，行按 owner_module 归属。
// 迁移工具（T08）用 CheckTable 核对每个迁移创建的表；docs/contracts/ownership.md
// 由本登记生成。
package ownership

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Database 是五个 SQLite 库之一。
type Database string

const (
	Main    Database = "main"
	Ledger  Database = "ledger"
	Runtime Database = "runtime"
	Events  Database = "events"
	Index   Database = "index"
)

// Module 是服务端模块及其所属任务。
type Module struct {
	Name    string
	Task    string
	Summary string
}

// Modules 列出全部模块；新增模块须先在此登记。
var Modules = []Module{
	{"identity", "T01", "身份、成员、角色、策略、凭据、Challenge/HumanGrant；会话运行记录"},
	{"catalog", "T02", "项目与资产稳定 ID、路径别名、著录修订、类型 schema、清单规范化"},
	{"storage", "T02", "Blob、上传会话与分片、BlobGrant/ReadGrant、暂存、版本安装与下载"},
	{"ledger", "T03", "版本登记与占名、审定、发布、锁定、回收站与墓碑、讨论"},
	{"provenance", "T03", "uses、许可快照、追加更正与限制计算的生效修订"},
	{"events", "T04", "事件收录、全局序号、审计导出与保留水位"},
	{"query", "T04", "目录、检索与关联投影；收件箱已读位置"},
	{"tasks", "T05", "Task/Seat/Attempt、任务租约与 fence、签出、指派与交接"},
	{"workflow", "T05", "业务 Flow、StepRun、输入锁定与转移去重"},
	{"agent_execution", "T06", "TaskRun、AgentStepRun、adapter 映射、预算、检查点与工具回执"},
	{"jobs", "T06", "Job、JobAttempt、作业租约与派发"},
	{"node", "T06", "节点在线状态、能力观测与资源租约"},
	{"extensions", "T09", "扩展登记与启用配置、调用与激活代次、熔断"},
	{"operations", "T08", "配置、迁移编排、恢复分派、备份清单、GC 调度"},
	{"commands", "T00", "共享命令组件：回执、operations、outbox 基础设施表"},
}

// InfraTable 是由共享组件维护、行按模块归属的基础设施表。
type InfraTable struct {
	Name      string
	Owner     string // 维护表结构与写入代码的组件
	Databases []Database
	Summary   string
}

// InfraTables 列出允许的基础设施表。
var InfraTables = []InfraTable{
	{"command_receipts", "commands", []Database{Main, Ledger, Runtime}, "命令回执，按 owner_module 归属"},
	{"operations", "commands", []Database{Main, Ledger, Runtime}, "持久操作阶段，按 owner_module 归属"},
	{"outbox", "commands", []Database{Main, Ledger, Runtime}, "源事件，与业务变更同事务写入"},
	{"processed_events", "events", []Database{Main, Ledger, Runtime, Index}, "消费者去重记录，与消费者状态同事务"},
	{"pending_commands", "events", []Database{Main, Ledger, Runtime, Index}, "消费者待执行的跨模块命令"},
	{"schema_migrations", "operations", []Database{Main, Ledger, Runtime, Events, Index}, "各库迁移版本"},
	{"instance_binding", "operations", []Database{Main, Ledger, Runtime, Events, Index}, "库所属实例：启动时核对，防止混用其他实例的库文件（同一实例旧副本的混用由备份清单与恢复流程核对）"},
}

// DatabaseRule 规定每个库允许哪些模块拥有业务表。
type DatabaseRule struct {
	Database Database
	Owners   []string
	Summary  string
}

// Databases 是五库的归属规则。
var Databases = []DatabaseRule{
	{Main, []string{"identity", "extensions"}, "身份、权限、策略、敏感授权与扩展登记/启用配置"},
	{Ledger, []string{"ledger", "provenance"}, "版本登记、审定、发布、锁定、生命周期与限制生效修订"},
	{Runtime, []string{"identity", "storage", "tasks", "workflow", "agent_execution", "jobs", "node", "extensions", "query"},
		"会话、上传与内容授权、任务与租约、流程、执行与作业、扩展激活、收件箱已读位置"},
	{Events, []string{"events"}, "事件收录、全局序号与审计水位"},
	{Index, []string{"query"}, "可删除重建的投影"},
}

// FileArea 是数据目录中的逻辑区域；具体路径布局由 storage 与 operations 决定。
type FileArea struct {
	Name    string
	Owner   string
	Summary string
}

// FileAreas 列出文件区域及写入者。
var FileAreas = []FileArea{
	{"blobs", "storage", "内容寻址的原件，不可变"},
	{"upload-staging", "storage", "上传暂存与分片；未校验内容不可引用"},
	{"install-staging", "storage", "版本私有安装区；台账 committed 前不可读"},
	{"versions", "storage", "不可变版本目录与 manifest；清单格式与规范化由 catalog 定义"},
	{"quarantine", "storage", "隔离的安装内容，保留字节待处置"},
	{"catalog", "catalog", "project.yaml、asset.yaml 修订与别名历史"},
	{"records", "provenance", "追加写入的来源、许可、检查与质检证据"},
	{"trash", "storage", "回收站文件；移动与清除按 ledger 持久化的意图执行"},
	{"audit", "events", "审计 JSONL 与摘要清单"},
	{"backups", "operations", "备份清单与复制目标"},
	{"extension-private", "extensions", "扩展私有命名空间目录；不是五库业务表"},
	{"instance", "operations", "实例标记 instance.json、单实例锁 lantai.lock 与 config.yaml"},
	{"secrets", "identity", "主密钥等密钥材料；与五库、事件、素材分开存放与备份"},
}

// EventPrefixes 把事件类型的第一段登记到所属模块；新前缀须先登记。
var EventPrefixes = map[string]string{
	"principal": "identity", "session": "identity", "project": "identity", "human_grant": "identity", "policy": "identity",
	"asset": "catalog", "namespace": "ledger",
	"upload": "storage", "blob_grant": "storage",
	"version": "ledger", "review": "ledger", "publication": "ledger", "lock": "ledger", "trash": "ledger", "comment": "ledger",
	"rights": "provenance",
	"task":   "tasks", "attempt": "tasks", "checkout": "tasks",
	"flow": "workflow", "step_run": "workflow",
	"task_run": "agent_execution", "job": "jobs", "node": "node",
	"extension": "extensions", "instance": "operations", "backup": "operations",
}

func moduleExists(name string) bool {
	return slices.ContainsFunc(Modules, func(m Module) bool { return m.Name == name })
}

// CheckTable 核对 table 是否可以出现在 db 中，返回写入者（模块或基础设施组件）。
func CheckTable(db Database, table string) (string, error) {
	if strings.HasPrefix(table, "sqlite_") {
		return "sqlite", nil // SQLite 内部表
	}
	for _, it := range InfraTables {
		if it.Name == table {
			if !slices.Contains(it.Databases, db) {
				return "", fmt.Errorf("ownership: infrastructure table %s is not allowed in %s.db", table, db)
			}
			return it.Owner, nil
		}
	}
	var rule *DatabaseRule
	for i := range Databases {
		if Databases[i].Database == db {
			rule = &Databases[i]
		}
	}
	if rule == nil {
		return "", fmt.Errorf("ownership: unknown database %q", db)
	}
	owner := ""
	for _, m := range Modules {
		if strings.HasPrefix(table, m.Name+"_") && len(m.Name) > len(owner) {
			owner = m.Name
		}
	}
	if owner == "" {
		return "", fmt.Errorf("ownership: table %s must be prefixed with its owning module", table)
	}
	if !slices.Contains(rule.Owners, owner) {
		return "", fmt.Errorf("ownership: module %s may not own tables in %s.db", owner, db)
	}
	return owner, nil
}

// EventOwner 返回事件类型的所属模块。
func EventOwner(eventType string) (string, error) {
	prefix, _, ok := strings.Cut(eventType, ".")
	if !ok {
		return "", fmt.Errorf("ownership: event type %q has no prefix", eventType)
	}
	owner, ok := EventPrefixes[prefix]
	if !ok {
		return "", fmt.Errorf("ownership: event prefix %q is not registered", prefix)
	}
	return owner, nil
}

// Validate 检查登记自身的一致性。
func Validate() error {
	seen := map[string]bool{}
	for _, m := range Modules {
		if seen[m.Name] {
			return fmt.Errorf("ownership: duplicate module %s", m.Name)
		}
		seen[m.Name] = true
	}
	for _, d := range Databases {
		for _, o := range d.Owners {
			if !moduleExists(o) {
				return fmt.Errorf("ownership: %s.db owner %s is not a module", d.Database, o)
			}
		}
	}
	for _, it := range InfraTables {
		if !moduleExists(it.Owner) {
			return fmt.Errorf("ownership: infra table %s owner %s is not a module", it.Name, it.Owner)
		}
		for _, m := range Modules {
			if strings.HasPrefix(it.Name, m.Name+"_") {
				return fmt.Errorf("ownership: infra table %s collides with module prefix %s_", it.Name, m.Name)
			}
		}
	}
	for _, f := range FileAreas {
		if !moduleExists(f.Owner) {
			return fmt.Errorf("ownership: file area %s owner %s is not a module", f.Name, f.Owner)
		}
	}
	prefixes := make([]string, 0, len(EventPrefixes))
	for p := range EventPrefixes {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)
	for _, p := range prefixes {
		if !moduleExists(EventPrefixes[p]) {
			return fmt.Errorf("ownership: event prefix %s owner %s is not a module", p, EventPrefixes[p])
		}
	}
	return nil
}
