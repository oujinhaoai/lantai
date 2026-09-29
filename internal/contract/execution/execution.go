// Package execution 实现业务执行（T05/T06）与扩展宿主（T09）共用的协议约定：
// 协议族与动作边界、任务 fence 与扩展激活代次的接受检查、恢复代次的错误
// 映射、TaskRun/宿主健康状态的分离，以及工具副作用的恢复规则。
//
// 枚举值与 schemas/common/v1/execution.schema.json 一致（测试核对）。本包
// 只定义判定规则，不启动任务服务、Agent 后端或插件 loader；这些在 M2/M3
// 由所属模块按同一规则接入。
package execution

import (
	"fmt"
	"slices"
)

// Protocol 是执行相关的协议族标识。
type Protocol string

const (
	// AgentExecution 是任务内 Agent 执行的业务协议（adapter：describe/start/…）。
	AgentExecution Protocol = "lantai.agent-execution/v1"
	// Processor 是一次性处理器文件协议：job.json → spawn/run/exit → out/result.json，
	// 没有任何控制动作。
	Processor Protocol = "lantai.processor/v1"
	// HostControl 是常驻实例的控制协议，按真实需求实现；不套用在一次性处理器上，
	// 也不替代业务执行协议。
	HostControl Protocol = "lantai.host-control/v1"
)

// Protocols 列出全部协议族。
var Protocols = []Protocol{AgentExecution, Processor, HostControl}

// Support 描述协议在当前构建中的状态。
type Support struct {
	Protocol Protocol `json:"protocol"`
	// Status：contract_only 表示只有契约与校验，没有运行中的服务；
	// enabled 表示存在限定范围的运行实现；reserved 表示只保留标识。
	Status string `json:"status"`
	// Planned 是计划启用阶段。
	Planned string `json:"planned"`
}

// SupportMatrix 报告构建能力；实例范围仍由 /meta 和实时授权决定。
func SupportMatrix() []Support {
	return []Support{
		{AgentExecution, "enabled", "manual_cli only; managed runner deferred"},
		{Processor, "enabled", "builtin corecheck only; external packages deferred"},
		{HostControl, "reserved", "on demand, resident instances only"},
	}
}

// ExecutionAction 是 lantai.agent-execution/v1 的动作。
type ExecutionAction string

const (
	ActDescribe ExecutionAction = "describe"
	ActStart    ExecutionAction = "start"
	ActLookup   ExecutionAction = "lookup"
	ActStatus   ExecutionAction = "status"
	ActResume   ExecutionAction = "resume"
	ActCancel   ExecutionAction = "cancel"
	ActResult   ExecutionAction = "result"
)

// ExecutionActions 是业务执行协议的全部动作。
var ExecutionActions = []ExecutionAction{ActDescribe, ActStart, ActLookup, ActStatus, ActResume, ActCancel, ActResult}

// HostControlAction 是 lantai.host-control/v1 的动作，只用于常驻实例。
type HostControlAction string

const (
	HostDescribe  HostControlAction = "describe"
	HostNegotiate HostControlAction = "negotiate"
	HostConfigure HostControlAction = "configure"
	HostHealth    HostControlAction = "health"
	HostDrain     HostControlAction = "drain"
	HostStop      HostControlAction = "stop"
)

// HostControlActions 是常驻控制协议的全部动作。
var HostControlActions = []HostControlAction{HostDescribe, HostNegotiate, HostConfigure, HostHealth, HostDrain, HostStop}

// ParseExecutionAction 只接受业务执行动作；health、drain 等宿主控制动作不能
// 借执行协议下发，反之亦然。
func ParseExecutionAction(s string) (ExecutionAction, error) {
	a := ExecutionAction(s)
	if !slices.Contains(ExecutionActions, a) {
		return "", fmt.Errorf("execution: %q is not an action of %s", s, AgentExecution)
	}
	return a, nil
}

// ParseHostControlAction 只接受常驻控制动作。
func ParseHostControlAction(s string) (HostControlAction, error) {
	a := HostControlAction(s)
	if !slices.Contains(HostControlActions, a) {
		return "", fmt.Errorf("execution: %q is not an action of %s", s, HostControl)
	}
	return a, nil
}

// ControlActions 返回协议允许的控制动作名；一次性处理器协议没有控制动作。
func ControlActions(p Protocol) []string {
	switch p {
	case AgentExecution:
		out := make([]string, len(ExecutionActions))
		for i, a := range ExecutionActions {
			out[i] = string(a)
		}
		return out
	case HostControl:
		out := make([]string, len(HostControlActions))
		for i, a := range HostControlActions {
			out[i] = string(a)
		}
		return out
	}
	return nil
}
