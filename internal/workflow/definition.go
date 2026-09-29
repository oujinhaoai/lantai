package workflow

import (
	"encoding/json"
	"slices"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
	"github.com/oujinhaoai/lantai/internal/identity"
)

// Definition 是 lantai.flow-definition/v1：作为 config 资产清单的
// metadata.flow_definition 保存，流程实例按精确版本与摘要固定。
type Definition struct {
	Contract string    `json:"contract"`
	Key      string    `json:"key"`
	Version  int       `json:"version"`
	Kind     string    `json:"kind"`
	Steps    []StepDef `json:"steps"`
}

type StepDef struct {
	Key    string         `json:"key"`
	Kind   string         `json:"kind"`
	Task   *TaskStepDef   `json:"task,omitempty"`
	Job    *JobStepDef    `json:"job,omitempty"`
	Review *ReviewStepDef `json:"review,omitempty"`
}

type TaskStepDef struct {
	Type         string        `json:"type"`
	Role         identity.Role `json:"role,omitempty"`
	Checkout     string        `json:"checkout,omitempty"`
	DistinctFrom string        `json:"distinct_from,omitempty"`
	Priority     string        `json:"priority,omitempty"`
}

type JobStepDef struct {
	Processor string `json:"processor"`
}

type ReviewStepDef struct {
	MaxRework *int `json:"max_rework,omitempty"`
}

const defaultMaxRework = 3

// ParseDefinition 按唯一 schema 与 M2 固定顺序校验定义，返回规范化摘要。
func ParseDefinition(v any) (Definition, digest.Digest, error) {
	var d Definition
	raw, err := canonjson.CanonicalizeValue(v)
	if err != nil {
		return d, "", errcode.Wrap(errcode.SchemaInvalid, "invalid flow definition", err)
	}
	doc, err := canonjson.Decode(raw)
	if err != nil {
		return d, "", errcode.Wrap(errcode.SchemaInvalid, "invalid flow definition", err)
	}
	reg, err := schema.Default()
	if err != nil {
		return d, "", err
	}
	if err = reg.Validate("lantai.flow-definition/v1", doc); err != nil {
		return d, "", errcode.Wrap(errcode.SchemaInvalid, "invalid flow definition", err)
	}
	if err = json.Unmarshal(raw, &d); err != nil {
		return d, "", err
	}
	if err = d.validate(); err != nil {
		return d, "", err
	}
	return d, digest.Of(raw), nil
}

// validate 只接受 制作 →（检查 job）→（质检 qa，独立于制作）→ 审定 → 发布。
// 并行、分支、汇合与子流程属于 M3 后续范围，明确拒绝。
func (d Definition) validate() error {
	bad := func(reason string) error {
		return errcode.New(errcode.UnsupportedCapability, "").WithDetails(errcode.Detail{Reason: reason})
	}
	keys := map[string]bool{}
	for _, s := range d.Steps {
		if keys[s.Key] {
			return bad("duplicate_step_key")
		}
		keys[s.Key] = true
	}
	i := 0
	first := d.Steps[0]
	if first.Kind != "task" || first.Task.Type == "qa" {
		return bad("first_step_must_produce")
	}
	if d.Kind == "modify" && (first.Task.Checkout == "" || first.Task.Checkout == "none") {
		return bad("modify_requires_checkout")
	}
	i++
	if i < len(d.Steps) && d.Steps[i].Kind == "job" {
		i++
	}
	if i < len(d.Steps) && d.Steps[i].Kind == "task" {
		qa := d.Steps[i].Task
		if qa.Type != "qa" || qa.DistinctFrom != first.Key || qa.Checkout != "" && qa.Checkout != "none" {
			return bad("qa_step_must_be_independent_of_production")
		}
		i++
	}
	if i >= len(d.Steps) || d.Steps[i].Kind != "review" {
		return bad("review_step_required")
	}
	i++
	if i != len(d.Steps)-1 || d.Steps[i].Kind != "publish" {
		return bad("publish_must_follow_review_last")
	}
	return nil
}

func (d Definition) index(key string) int {
	return slices.IndexFunc(d.Steps, func(s StepDef) bool { return s.Key == key })
}

func (d Definition) maxRework() int {
	for _, s := range d.Steps {
		if s.Kind == "review" && s.Review != nil && s.Review.MaxRework != nil {
			return *s.Review.MaxRework
		}
	}
	return defaultMaxRework
}

// BuiltinDefinition 返回 M2 内置入库、新建与修改流程的推荐定义。它只是模板：
// 实例必须引用已入库的 config 版本，不能凭内置名称运行。
func BuiltinDefinition(kind string) (Definition, error) {
	produce := map[string]string{"ingest": "ingest", "create": "produce", "modify": "produce"}[kind]
	if produce == "" {
		return Definition{}, invalid("unknown built-in flow kind")
	}
	// 新建与入库首轮尚无资产，返工轮次签出首轮产出的资产；修改流程首轮签出目标资产。
	checkout := "exclusive"
	max := defaultMaxRework
	return Definition{Contract: "lantai.flow-definition/v1", Key: "lantai." + kind, Version: 1, Kind: kind, Steps: []StepDef{
		{Key: "produce", Kind: "task", Task: &TaskStepDef{Type: produce, Role: identity.RoleContributor, Checkout: checkout}},
		{Key: "check", Kind: "job", Job: &JobStepDef{Processor: "org.lantai.corecheck.manifest"}},
		{Key: "qa", Kind: "task", Task: &TaskStepDef{Type: "qa", Role: identity.RoleChecker, DistinctFrom: "produce"}},
		{Key: "review", Kind: "review", Review: &ReviewStepDef{MaxRework: &max}},
		{Key: "publish", Kind: "publish"},
	}}, nil
}
