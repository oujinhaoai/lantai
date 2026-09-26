package commands

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// HashContract 是请求摘要规则的契约标识，参与摘要计算；规则变化必须换新版本。
const HashContract = "lantai.request-hash/v1"

// HashInput 是参与 request_hash 的全部内容。
//
// 摘要覆盖规范化的命令体、路径目标、预期修订与输入版本，不覆盖 bearer token、
// trace/request ID 等传输细节；actor 已在幂等键作用域中，不重复参与。
// Body 必须是所属命令完成领域规范化（例如路径 NFC）之后的表示；同一语义
// 请求的重试必须得到相同 Body。
type HashInput struct {
	CommandType       string
	ProjectID         ids.ID
	Targets           []string
	ExpectedRevisions map[string]int64
	InputVersions     []string
	Body              json.RawMessage
}

type hashDoc struct {
	Contract          string           `json:"contract"`
	CommandType       string           `json:"command_type"`
	ProjectID         ids.ID           `json:"project_id,omitempty"`
	Targets           []string         `json:"targets"`
	ExpectedRevisions map[string]int64 `json:"expected_revisions"`
	InputVersions     []string         `json:"input_versions"`
	Body              json.RawMessage  `json:"body"`
}

// ErrHashInput 表示摘要输入不合法。
var ErrHashInput = errors.New("commands: invalid request hash input")

// RequestHash 按 RFC 8785 规范化后计算 "sha256:<hex>"。
// Targets 与 InputVersions 保持调用方给出的顺序（顺序有语义时由命令固定）；
// 字段缺省与空集合等价。
func RequestHash(in HashInput) (digest.Digest, error) {
	if !commandTypeRE.MatchString(in.CommandType) {
		return "", fmt.Errorf("%w: command_type %q", ErrHashInput, in.CommandType)
	}
	if in.ProjectID != "" && !in.ProjectID.Valid() {
		return "", fmt.Errorf("%w: project_id", ErrHashInput)
	}
	body := in.Body
	if len(body) == 0 {
		body = json.RawMessage("null")
	}
	if err := canonjson.Validate(body); err != nil {
		return "", fmt.Errorf("%w: body: %v", ErrHashInput, err)
	}
	doc := hashDoc{
		Contract:          HashContract,
		CommandType:       in.CommandType,
		ProjectID:         in.ProjectID,
		Targets:           nonNil(in.Targets),
		ExpectedRevisions: in.ExpectedRevisions,
		InputVersions:     nonNil(in.InputVersions),
		Body:              body,
	}
	if doc.ExpectedRevisions == nil {
		doc.ExpectedRevisions = map[string]int64{}
	}
	canonical, err := canonjson.CanonicalizeValue(doc)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrHashInput, err)
	}
	return digest.Of(canonical), nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
