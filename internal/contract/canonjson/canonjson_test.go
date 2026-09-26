package canonjson

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

// RFC 8785 §3.2.2.3 的双精度输出样例（按 IEEE-754 位模式给出）。
func TestFormatNumberRFC8785(t *testing.T) {
	cases := []struct {
		bits uint64
		want string
	}{
		{0x0000000000000000, "0"},
		{0x8000000000000000, "0"},
		{0x0000000000000001, "5e-324"},
		{0x8000000000000001, "-5e-324"},
		{0x7fefffffffffffff, "1.7976931348623157e+308"},
		{0xffefffffffffffff, "-1.7976931348623157e+308"},
		{0x4340000000000000, "9007199254740992"},
		{0xc340000000000000, "-9007199254740992"},
		{0x4430000000000000, "295147905179352830000"},
		{0x44b52d02c7e14af5, "9.999999999999997e+22"},
		{0x44b52d02c7e14af6, "1e+23"},
		{0x44b52d02c7e14af7, "1.0000000000000001e+23"},
		{0x444b1ae4d6e2ef4e, "999999999999999700000"},
		{0x444b1ae4d6e2ef4f, "999999999999999900000"},
		{0x444b1ae4d6e2ef50, "1e+21"},
		{0x3eb0c6f7a0b5ed8c, "9.999999999999997e-7"},
		{0x3eb0c6f7a0b5ed8d, "0.000001"},
		{0x41b3de4355555553, "333333333.3333332"},
		{0x41b3de4355555554, "333333333.33333325"},
		{0x41b3de4355555555, "333333333.3333333"},
		{0x41b3de4355555556, "333333333.3333334"},
		{0x41b3de4355555557, "333333333.33333343"},
		{0xbecbf647612f3696, "-0.0000033333333333333333"},
		{0x43143ff3c1cb0959, "1424953923781206.2"},
	}
	for _, c := range cases {
		if got := FormatNumber(math.Float64frombits(c.bits)); got != c.want {
			t.Errorf("FormatNumber(%#x) = %s, want %s", c.bits, got, c.want)
		}
	}
}

// esc 把测试输入里的 "%u" 换成 JSON 的反斜杠 u 转义，避免在源码里直接书写该转义序列。
func esc(s string) string { return strings.ReplaceAll(s, "%u", string(rune(92))+"u") }

// RFC 8785 §3.2.2 的完整示例。
func TestCanonicalizeRFCExample(t *testing.T) {
	in := esc(`{
  "numbers": [333333333.33333329, 1E30, 4.50, 2e-3, 0.000000000000000000000000001],
  "string": "%u20ac$%u000F%u000aA'%u0042%u0022%u005cBSBSBSQ\/",
  "literals": [null, true, false]
}`)
	in = strings.ReplaceAll(in, "BSBSBSQ", `\\\"`)
	euro := string(rune(0x20AC))
	want := `{"literals":[null,true,false],"numbers":[333333333.3333333,1e+30,4.5,0.002,1e-27],"string":"` + euro + esc(`$%u000f`) + `\nA'B\"\\\\\"/"}`
	got, err := Canonicalize([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// RFC 8785 §3.2.3：按 UTF-16 码元排序，表情符号排在 U+FB33 之前。
func TestCanonicalizeSortsByUTF16(t *testing.T) {
	in := esc(`{"%u20ac":"Euro Sign","\r":"Carriage Return","%ufb33":"Hebrew Letter Dalet With Dagesh","1":"One","%ud83d%ude00":"Emoji: Grinning Face","%u0080":"Control","%u00f6":"Latin Small Letter O With Diaeresis"}`)
	got, err := Canonicalize([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	dec := json.NewDecoder(strings.NewReader(string(got)))
	tok, _ := dec.Token()
	if tok != json.Delim('{') {
		t.Fatalf("unexpected token %v", tok)
	}
	for dec.More() {
		k, _ := dec.Token()
		keys = append(keys, k.(string))
		var v any
		_ = dec.Decode(&v)
	}
	want := []string{"\r", "1", string(rune(0x80)), string(rune(0xF6)), string(rune(0x20AC)), string(rune(0x1F600)), string(rune(0xFB33))}
	if strings.Join(keys, "|") != strings.Join(want, "|") {
		t.Fatalf("key order %q, want %q", keys, want)
	}
}

func TestCanonicalizeIsStableAcrossFormatting(t *testing.T) {
	a := `{"b":[1,2,{"y":true,"x":null}],"a":"A"}`
	b := "{ \"a\" : \"A\" ,\n \"b\" : [ 1.0, 2E0, { \"x\": null, \"y\": true } ] }"
	ca, err := Canonicalize([]byte(a))
	if err != nil {
		t.Fatal(err)
	}
	cb, err := Canonicalize([]byte(b))
	if err != nil {
		t.Fatal(err)
	}
	if string(ca) != string(cb) {
		t.Fatalf("%s != %s", ca, cb)
	}
	again, _ := Canonicalize(ca)
	if string(again) != string(ca) {
		t.Fatal("canonical form is not a fixed point")
	}
}

func TestStrictRejections(t *testing.T) {
	cases := map[string]string{
		"duplicate key":        `{"a":1,"a":2}`,
		"duplicate after esc":  esc(`{"a":1,"%u0061":2}`),
		"lone high surrogate":  esc(`"%ud800"`),
		"lone low surrogate":   esc(`"%udc00"`),
		"bad pair":             esc(`"%ud800%u0041"`),
		"invalid utf8":         "\"\xff\"",
		"control char":         "\"a\x01b\"",
		"trailing data":        `{} {}`,
		"trailing comma":       `[1,]`,
		"leading zero":         `01`,
		"bare fraction":        `1.`,
		"plus sign":            `+1`,
		"nan":                  `NaN`,
		"huge float":           `1e400`,
		"unsafe integer":       `9007199254740992`,
		"unsafe negative":      `-9007199254740993`,
		"single quotes":        `{'a':1}`,
		"bad escape":           `"\x"`,
		"empty":                ``,
		"unterminated string":  `"abc`,
		"unterminated object":  `{"a":1`,
		"missing colon":        `{"a" 1}`,
		"non string key":       `{1:2}`,
		"truncated literal":    `tru`,
		"truncated unicode":    `"\u12"`,
		"deep nesting":         strings.Repeat("[", MaxDepth+1) + strings.Repeat("]", MaxDepth+1),
		"comment":              `{"a":1 /* c */}`,
		"trailing obj comma":   `{"a":1,}`,
		"exponent without dig": `1e`,
	}
	for name, in := range cases {
		if _, err := Canonicalize([]byte(in)); err == nil {
			t.Errorf("%s: accepted %q", name, in)
		}
	}
}

func TestAcceptsBoundaries(t *testing.T) {
	for in, want := range map[string]string{
		`9007199254740991`:   "9007199254740991",
		`-9007199254740991`:  "-9007199254740991",
		`9007199254740992.0`: "9007199254740992", // 带小数点的按浮点数语义处理
		`-0`:                 "0",
		`1E2`:                "100",
		`"\u007f"`:           "\"\u007f\"",
		strings.Repeat("[", MaxDepth) + strings.Repeat("]", MaxDepth): strings.Repeat("[", MaxDepth) + strings.Repeat("]", MaxDepth),
	} {
		got, err := Canonicalize([]byte(in))
		if err != nil {
			t.Errorf("%q rejected: %v", in, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%q -> %s, want %s", in, got, want)
		}
	}
}

func TestLineSeparatorIsNotEscaped(t *testing.T) {
	got, err := Canonicalize([]byte(esc(`"%u2028"`)))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "\""+string(rune(0x2028))+"\"" {
		t.Fatalf("got %q", got)
	}
}

func TestDecodeKeepsNumberLiteral(t *testing.T) {
	v, err := Decode([]byte(`{"n":1.50,"s":"x","a":[true,null]}`))
	if err != nil {
		t.Fatal(err)
	}
	m := v.(map[string]any)
	if m["n"] != json.Number("1.50") {
		t.Fatalf("number literal = %#v", m["n"])
	}
	if m["s"] != "x" {
		t.Fatalf("string = %#v", m["s"])
	}
	a := m["a"].([]any)
	if a[0] != true || a[1] != nil {
		t.Fatalf("array = %#v", a)
	}
}

func TestCanonicalizeValue(t *testing.T) {
	got, err := CanonicalizeValue(map[string]any{"z": "<&>", "a": []int{3, 1}})
	if err != nil {
		t.Fatal(err)
	}
	// encoding/json 会把 <&> 转义成反斜杠 u 形式，规范化后恢复为原字符。
	if string(got) != `{"a":[3,1],"z":"<&>"}` {
		t.Fatalf("got %s", got)
	}
}

// encoding/json 会把非法 UTF-8 替换为 U+FFFD；规范化 Go 值时必须先拒绝。
func TestCanonicalizeValueRejectsInvalidUTF8(t *testing.T) {
	type inner struct {
		Name string
		Raw  json.RawMessage
	}
	for name, v := range map[string]any{
		"string":      "a\xff",
		"map key":     map[string]int{"k\xfe": 1},
		"map value":   map[string]string{"k": "\xff"},
		"slice":       []string{"ok", "a\xfe"},
		"struct":      inner{Name: "\xc0"},
		"raw message": inner{Name: "ok", Raw: json.RawMessage("\"\xff\"")},
		"pointer":     &inner{Name: "\xff"},
	} {
		if _, err := CanonicalizeValue(v); !errors.Is(err, ErrInvalidUTF8) {
			t.Errorf("%s: %v", name, err)
		}
	}
	replacement := string(rune(0xFFFD))
	a, err := CanonicalizeValue([]string{"a" + replacement})
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != "[\"a"+replacement+"\"]" {
		t.Fatalf("a real U+FFFD must stay valid: %s", a)
	}
	type hidden struct {
		secret string // 未导出字段不参与 JSON 编码
		Shown  string
	}
	if _, err := CanonicalizeValue(hidden{secret: "\xff", Shown: "ok"}); err != nil {
		t.Fatalf("unexported fields are not encoded and must be ignored: %v", err)
	}
}
