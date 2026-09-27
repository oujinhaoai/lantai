package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
	"github.com/oujinhaoai/lantai/internal/contract/yamljson"
)

// 说明与清单文件用 YAML 保存，便于人直接阅读；内容按 JSON 数据模型解释，
// 摘要按 RFC 8785 规范化计算，与写成 YAML 还是 JSON 无关。渲染是确定性的：
// 同一文档总是得到相同的字节，重试不会改变摘要。

// KeyOrder 给出某个映射（按 JSON Pointer 定位）优先输出的键；未列出的键按
// 字典序排在后面。
type KeyOrder func(pointer string) []string

// Render 把可按 encoding/json 编码的值渲染为确定性的 YAML（UTF-8、LF、两格缩进）。
// 数字保留规范化后的字面量，字符串在需要时加引号，读回后与原值的规范化 JSON 相同。
func Render(v any, order KeyOrder) ([]byte, error) {
	if err := canonjson.CheckUTF8(v); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	canonical, err := canonjson.Canonicalize(raw)
	if err != nil {
		return nil, err
	}
	tree, err := canonjson.Decode(canonical)
	if err != nil {
		return nil, err
	}
	node, err := toNode(tree, "", order)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(node); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func toNode(v any, pointer string, order KeyOrder) (*yaml.Node, error) {
	switch x := v.(type) {
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: fmt.Sprint(x)}, nil
	case json.Number:
		tag := "!!int"
		if strings.ContainsAny(string(x), ".eE") {
			tag = "!!float"
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: string(x)}, nil
	case string:
		n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: x}
		if strings.Contains(x, "\n") {
			n.Style = yaml.DoubleQuotedStyle // 保留换行与行尾空白的精确值
		}
		return n, nil
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for i, item := range x {
			c, err := toNode(item, fmt.Sprintf("%s/%d", pointer, i), order)
			if err != nil {
				return nil, err
			}
			n.Content = append(n.Content, c)
		}
		if len(x) == 0 {
			n.Style = yaml.FlowStyle
		}
		return n, nil
	case map[string]any:
		n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var preferred []string
		if order != nil {
			preferred = order(pointer)
		}
		sort.SliceStable(keys, func(i, j int) bool {
			pi, pj := slices.Index(preferred, keys[i]), slices.Index(preferred, keys[j])
			switch {
			case pi >= 0 && pj >= 0:
				return pi < pj
			case pi >= 0:
				return true
			case pj >= 0:
				return false
			}
			return false
		})
		for _, k := range keys {
			c, err := toNode(x[k], pointer+"/"+escapePointer(k), order)
			if err != nil {
				return nil, err
			}
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, c)
		}
		if len(x) == 0 {
			n.Style = yaml.FlowStyle
		}
		return n, nil
	}
	return nil, fmt.Errorf("manifest: unsupported value %T", v)
}

func escapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// Decode 解析 YAML（或 JSON）文档，按契约 schema 校验后返回 JSON 数据模型的值。
func Decode(raw []byte, contract string) (any, error) {
	tree, err := yamljson.Decode(raw)
	if err != nil {
		return nil, err
	}
	reg, err := schema.Default()
	if err != nil {
		return nil, err
	}
	if err := reg.Validate(contract, tree); err != nil {
		return nil, err
	}
	return tree, nil
}

// canonicalOf 返回 JSON 数据模型值的 RFC 8785 规范化字节。
func canonicalOf(tree any) ([]byte, error) {
	raw, err := json.Marshal(tree)
	if err != nil {
		return nil, err
	}
	return canonjson.Canonicalize(raw)
}
