// Package yamljson 按 JSON 数据模型解释 YAML 文档。
//
// asset.yaml、manifest.yaml、extension.yaml 等 YAML 文件的摘要和 schema
// 校验都基于其 JSON 数据模型，因此解释规则必须唯一：只允许单个文档；
// 映射键必须是字符串且不重复；拒绝锚点、别名、合并键和自定义标签；
// 整数只接受十进制且在 ±(2^53−1) 以内，浮点数必须符合 JSON 数字语法；
// 时间戳按原文作为字符串。解码结果与 canonjson.Decode 的值类型一致。
package yamljson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"

	"go.yaml.in/yaml/v3"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
)

// MaxDepth 与 canonjson 保持一致。
const MaxDepth = canonjson.MaxDepth

// Error 描述不符合规则的位置。
type Error struct {
	Line, Column int
	Reason       string
}

func (e *Error) Error() string {
	return fmt.Sprintf("yamljson: %s at line %d column %d", e.Reason, e.Line, e.Column)
}

func nodeErr(n *yaml.Node, format string, args ...any) error {
	return &Error{Line: n.Line, Column: n.Column, Reason: fmt.Sprintf(format, args...)}
}

var (
	jsonInt   = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)
	jsonFloat = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)
)

// Decode 解析单个 YAML 文档并返回 JSON 数据模型的值。
func Decode(data []byte) (any, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("yamljson: empty document")
		}
		return nil, fmt.Errorf("yamljson: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, nodeErr(&extra, "multiple YAML documents are not allowed")
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("yamljson: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, errors.New("yamljson: expected exactly one document")
	}
	return convert(doc.Content[0], 0)
}

// ToJSON 把 YAML 文档转换为等价的 JSON 字节（非规范化）。
func ToJSON(data []byte) ([]byte, error) {
	v, err := Decode(data)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// convert 转换节点；depth 是已经进入的容器层数。与 canonjson 一致，只有映射
// 和序列计入嵌套深度，标量叶子不计。
func convert(n *yaml.Node, depth int) (any, error) {
	if (n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode) && depth+1 > MaxDepth {
		return nil, nodeErr(n, "nesting deeper than %d", MaxDepth)
	}
	if n.Anchor != "" {
		return nil, nodeErr(n, "anchors are not allowed")
	}
	switch n.Kind {
	case yaml.AliasNode:
		return nil, nodeErr(n, "aliases are not allowed")
	case yaml.MappingNode:
		if n.Tag != "!!map" {
			return nil, nodeErr(n, "unsupported mapping tag %s", n.Tag)
		}
		out := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Kind != yaml.ScalarNode || k.Tag != "!!str" {
				if k.Tag == "!!merge" {
					return nil, nodeErr(k, "merge keys are not allowed")
				}
				return nil, nodeErr(k, "mapping keys must be strings (quote non-string keys)")
			}
			if k.Anchor != "" {
				return nil, nodeErr(k, "anchors are not allowed")
			}
			if _, dup := out[k.Value]; dup {
				return nil, nodeErr(k, "duplicate mapping key %q", k.Value)
			}
			cv, err := convert(v, depth+1)
			if err != nil {
				return nil, err
			}
			out[k.Value] = cv
		}
		return out, nil
	case yaml.SequenceNode:
		if n.Tag != "!!seq" {
			return nil, nodeErr(n, "unsupported sequence tag %s", n.Tag)
		}
		out := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			cv, err := convert(c, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, cv)
		}
		return out, nil
	case yaml.ScalarNode:
		return scalar(n)
	default:
		return nil, nodeErr(n, "unsupported YAML node kind %d", n.Kind)
	}
}

func scalar(n *yaml.Node) (any, error) {
	switch n.Tag {
	case "!!str":
		// yaml.v3 会把超出浮点范围的数字（如 1e400）解析为字符串；未加引号且
		// 符合 JSON 数字语法的标量一律按数字规则处理，越界即拒绝。
		if n.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) == 0 &&
			jsonFloat.MatchString(n.Value) {
			return number(n)
		}
		return n.Value, nil
	case "!!timestamp":
		return n.Value, nil
	case "!!null":
		return nil, nil
	case "!!bool":
		b, err := strconv.ParseBool(n.Value)
		if err != nil {
			return nil, nodeErr(n, "invalid boolean %q", n.Value)
		}
		return b, nil
	case "!!int", "!!float":
		// yaml.v3 会把超出 int64/uint64 的整数标成 !!float，所以按字面量形态
		// 判断，而不是按标签。
		return number(n)
	default:
		return nil, nodeErr(n, "unsupported scalar tag %s", n.Tag)
	}
}

// number 按 JSON 数字规则解释标量：整数字面量必须在 ±(2^53−1) 以内，
// 其他数字必须符合 JSON 语法且在双精度范围内，与 canonjson 相同。
func number(n *yaml.Node) (any, error) {
	switch {
	case jsonInt.MatchString(n.Value):
		f, err := strconv.ParseFloat(n.Value, 64)
		if err != nil || math.Abs(f) > canonjson.MaxSafeInteger {
			return nil, nodeErr(n, "integer %q outside ±(2^53-1)", n.Value)
		}
	case jsonFloat.MatchString(n.Value):
		f, err := strconv.ParseFloat(n.Value, 64)
		if err != nil || math.IsInf(f, 0) {
			return nil, nodeErr(n, "number %q out of range", n.Value)
		}
	default:
		return nil, nodeErr(n, "number %q must use JSON number notation", n.Value)
	}
	return json.Number(n.Value), nil
}
