package yamljson

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
)

func TestDecodeMatchesJSONDataModel(t *testing.T) {
	y := `
contract: lantai.extension/v1
id: org.example.asset-tools
version: 0.1.0
count: 2
ratio: 0.5
enabled: true
missing: null
when: 2026-09-26
tags: [a, "b"]
nested:
  "1": one
`
	got, err := Decode([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	m := got.(map[string]any)
	if m["count"] != json.Number("2") || m["ratio"] != json.Number("0.5") {
		t.Fatalf("numbers = %#v %#v", m["count"], m["ratio"])
	}
	if m["enabled"] != true || m["missing"] != nil {
		t.Fatalf("bool/null = %#v %#v", m["enabled"], m["missing"])
	}
	if m["when"] != "2026-09-26" || m["version"] != "0.1.0" {
		t.Fatalf("strings = %#v %#v", m["when"], m["version"])
	}

	// 与等价 JSON 的规范化结果逐字节一致。
	js, err := ToJSON([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	fromYAML, err := canonjson.Canonicalize(js)
	if err != nil {
		t.Fatal(err)
	}
	fromJSON, err := canonjson.Canonicalize([]byte(`{"contract":"lantai.extension/v1","id":"org.example.asset-tools","version":"0.1.0","count":2,"ratio":0.5,"enabled":true,"missing":null,"when":"2026-09-26","tags":["a","b"],"nested":{"1":"one"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(fromYAML) != string(fromJSON) {
		t.Fatalf("yaml %s\njson %s", fromYAML, fromJSON)
	}
}

func TestRejections(t *testing.T) {
	cases := map[string]string{
		"duplicate key":   "a: 1\na: 2\n",
		"anchor":          "a: &x 1\nb: 2\n",
		"alias":           "a: &x [1]\nb: *x\n",
		"merge key":       "base: {a: 1}\nderived:\n  <<: {a: 1}\n",
		"non string key":  "1: one\n",
		"bool key":        "true: yes\n",
		"hex int":         "a: 0x1F\n",
		"octal int":       "a: 0o17\n",
		"unsafe int":      "a: 9007199254740992\n",
		"inf":             "a: .inf\n",
		"nan":             "a: .nan\n",
		"custom tag":      "a: !secret value\n",
		"binary":          "a: !!binary aGVsbG8=\n",
		"multi document":  "a: 1\n---\nb: 2\n",
		"empty":           "",
		"syntax":          "a: [1, 2\n",
		"deep":            strings.Repeat("- ", MaxDepth+1) + "x\n",
		"leading dot":     "a: .5\n",
		"trailing dot":    "a: 1.\n",
		"null key":        "~: x\n",
		"sequence merged": "a: !!set {x}\n",
	}
	for name, y := range cases {
		if _, err := Decode([]byte(y)); err == nil {
			t.Errorf("%s: accepted %q", name, y)
		}
	}
}

func TestQuotedValuesStayStrings(t *testing.T) {
	got, err := Decode([]byte("a: \"0x1F\"\nb: 'yes'\nc: yes\nd: \"123\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	m := got.(map[string]any)
	for k, want := range map[string]any{"a": "0x1F", "b": "yes", "c": "yes", "d": "123"} {
		if m[k] != want {
			t.Errorf("%s = %#v, want %#v", k, m[k], want)
		}
	}
}

// YAML 与 JSON 对同一数据的接受范围必须一致。
func TestNumberAndDepthRulesMatchCanonjson(t *testing.T) {
	for _, bad := range []string{
		"n: 18446744073709551616\n", // 超出 uint64，yaml.v3 标为 !!float
		"n: -9223372036854775809\n", // 超出 int64
		"n: 9007199254740992\n",
		"n: 1e400\n", // yaml.v3 解析为字符串
		"n: -1e400\n",
	} {
		if _, err := Decode([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	for in, want := range map[string]any{
		"n: 1e5\n":                    json.Number("1e5"),
		"n: 9007199254740991\n":       json.Number("9007199254740991"),
		"n: \"1e400\"\n":              "1e400", // 加引号的仍是字符串
		"n: '18446744073709551616'\n": "18446744073709551616",
	} {
		v, err := Decode([]byte(in))
		if err != nil {
			t.Errorf("%q rejected: %v", in, err)
			continue
		}
		if got := v.(map[string]any)["n"]; got != want {
			t.Errorf("%q -> %#v, want %#v", in, got, want)
		}
	}
	// 128 层容器加标量叶子：JSON 与 YAML 都接受；129 层都拒绝。
	for _, levels := range []int{MaxDepth, MaxDepth + 1} {
		js := strings.Repeat("[", levels) + "1" + strings.Repeat("]", levels)
		_, jerr := canonjson.Canonicalize([]byte(js))
		_, yerr := Decode([]byte(js)) // 流式序列也是合法 YAML
		if (jerr == nil) != (yerr == nil) {
			t.Errorf("%d levels: json err %v, yaml err %v", levels, jerr, yerr)
		}
		if (levels <= MaxDepth) != (yerr == nil) {
			t.Errorf("%d levels: yaml err %v", levels, yerr)
		}
	}
}
