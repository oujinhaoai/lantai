// Package canonjson 实现兰台摘要使用的 JSON 规范化（RFC 8785 JCS）。
//
// request_hash、清单摘要等都对规范化后的字节求 SHA-256。输入先按
// I-JSON（RFC 7493）约束严格解析：必须是合法 UTF-8，拒绝重复键、孤立
// 代理项、未转义控制字符和超过嵌套上限的文档。数字按 IEEE-754 双精度
// 解释并按 ECMAScript 规则输出；为避免不同整数被舍入到同一个值，
// 不带小数点和指数的整数字面量必须在 ±(2^53−1) 以内。
// 本包不做 Unicode 规范化；路径等需要 NFC 的字段由领域命令先规范化。
package canonjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// MaxDepth 是允许的最大嵌套层数。
const MaxDepth = 128

// MaxSafeInteger 是整数字面量允许的最大绝对值（2^53−1）。
const MaxSafeInteger = 1<<53 - 1

// Error 描述输入不满足严格 JSON 约束的位置与原因。
type Error struct {
	Offset int
	Reason string
}

func (e *Error) Error() string {
	return fmt.Sprintf("canonjson: %s at byte offset %d", e.Reason, e.Offset)
}

type kind uint8

const (
	kNull kind = iota
	kBool
	kNumber
	kString
	kArray
	kObject
)

type member struct {
	key string
	val *node
}

type node struct {
	kind kind
	b    bool
	num  float64
	lit  string // 数字的原始字面量
	str  string
	arr  []*node
	obj  []member
}

// Canonicalize 严格解析 data 并返回 RFC 8785 规范化字节。
func Canonicalize(data []byte) ([]byte, error) {
	n, err := parse(data)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.Grow(len(data))
	writeNode(&buf, n)
	return buf.Bytes(), nil
}

// CanonicalizeValue 先用 encoding/json 编码 v，再规范化。encoding/json 会把
// 字符串中的非法 UTF-8 静默替换为 U+FFFD，使不同输入得到相同摘要；因此先用
// CheckUTF8 拒绝这类值。
func CanonicalizeValue(v any) ([]byte, error) {
	if err := CheckUTF8(v); err != nil {
		return nil, err
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("canonjson: marshal: %w", err)
	}
	return Canonicalize(data)
}

// ErrInvalidUTF8 表示 Go 值中含有非法 UTF-8 的字符串或 JSON 片段。
var ErrInvalidUTF8 = errors.New("canonjson: value contains invalid UTF-8")

var rawMessageType = reflect.TypeFor[json.RawMessage]()

// CheckUTF8 遍历 v 中会被 JSON 编码的字符串（导出字段、映射键与值、切片、
// 指针与接口）以及 json.RawMessage，发现非法 UTF-8 即返回 ErrInvalidUTF8。
func CheckUTF8(v any) error {
	return checkValue(reflect.ValueOf(v), 0)
}

func checkValue(v reflect.Value, depth int) error {
	if depth > MaxDepth*4 {
		return fmt.Errorf("canonjson: value nested too deeply")
	}
	if !v.IsValid() {
		return nil
	}
	if v.Type() == rawMessageType {
		if !utf8.Valid(v.Bytes()) {
			return ErrInvalidUTF8
		}
		return nil
	}
	switch v.Kind() {
	case reflect.String:
		if !utf8.ValidString(v.String()) {
			return ErrInvalidUTF8
		}
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			return checkValue(v.Elem(), depth+1)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				if err := checkValue(v.Field(i), depth+1); err != nil {
					return err
				}
			}
		}
	case reflect.Map:
		it := v.MapRange()
		for it.Next() {
			if err := checkValue(it.Key(), depth+1); err != nil {
				return err
			}
			if err := checkValue(it.Value(), depth+1); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8 {
			return nil // []byte 以 base64 编码，不涉及 UTF-8
		}
		for i := range v.Len() {
			if err := checkValue(v.Index(i), depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// Validate 只做严格解析，不产生输出。
func Validate(data []byte) error {
	_, err := parse(data)
	return err
}

// Decode 严格解析 data，返回 map[string]any、[]any、string、json.Number、
// bool 或 nil 组成的值；数字保留原始字面量，供 schema 校验使用。
func Decode(data []byte) (any, error) {
	n, err := parse(data)
	if err != nil {
		return nil, err
	}
	return toAny(n), nil
}

func toAny(n *node) any {
	switch n.kind {
	case kNull:
		return nil
	case kBool:
		return n.b
	case kNumber:
		return json.Number(n.lit)
	case kString:
		return n.str
	case kArray:
		out := make([]any, len(n.arr))
		for i, c := range n.arr {
			out[i] = toAny(c)
		}
		return out
	default:
		out := make(map[string]any, len(n.obj))
		for _, m := range n.obj {
			out[m.key] = toAny(m.val)
		}
		return out
	}
}

type parser struct {
	data  []byte
	pos   int
	depth int
}

func parse(data []byte) (*node, error) {
	if !utf8.Valid(data) {
		return nil, &Error{Offset: invalidUTF8Offset(data), Reason: "invalid UTF-8"}
	}
	p := &parser{data: data}
	p.skipSpace()
	n, err := p.value()
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.pos != len(p.data) {
		return nil, p.errf("unexpected trailing data")
	}
	return n, nil
}

func invalidUTF8Offset(data []byte) int {
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size <= 1 {
			return i
		}
		i += size
	}
	return len(data)
}

func (p *parser) errf(format string, args ...any) error {
	return &Error{Offset: p.pos, Reason: fmt.Sprintf(format, args...)}
}

func (p *parser) skipSpace() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *parser) value() (*node, error) {
	if p.pos >= len(p.data) {
		return nil, p.errf("unexpected end of input")
	}
	switch c := p.data[p.pos]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"':
		s, err := p.string()
		if err != nil {
			return nil, err
		}
		return &node{kind: kString, str: s}, nil
	case c == 't':
		return p.literal("true", &node{kind: kBool, b: true})
	case c == 'f':
		return p.literal("false", &node{kind: kBool})
	case c == 'n':
		return p.literal("null", &node{kind: kNull})
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	default:
		return nil, p.errf("unexpected character %q", c)
	}
}

func (p *parser) literal(word string, n *node) (*node, error) {
	if !bytes.HasPrefix(p.data[p.pos:], []byte(word)) {
		return nil, p.errf("invalid literal")
	}
	p.pos += len(word)
	return n, nil
}

func (p *parser) enter() error {
	p.depth++
	if p.depth > MaxDepth {
		return p.errf("nesting deeper than %d", MaxDepth)
	}
	return nil
}

func (p *parser) object() (*node, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer func() { p.depth-- }()
	p.pos++ // '{'
	n := &node{kind: kObject}
	seen := map[string]struct{}{}
	p.skipSpace()
	if p.pos < len(p.data) && p.data[p.pos] == '}' {
		p.pos++
		return n, nil
	}
	for {
		p.skipSpace()
		if p.pos >= len(p.data) || p.data[p.pos] != '"' {
			return nil, p.errf("expected object key")
		}
		keyPos := p.pos
		key, err := p.string()
		if err != nil {
			return nil, err
		}
		if _, dup := seen[key]; dup {
			return nil, &Error{Offset: keyPos, Reason: fmt.Sprintf("duplicate object key %q", key)}
		}
		seen[key] = struct{}{}
		p.skipSpace()
		if p.pos >= len(p.data) || p.data[p.pos] != ':' {
			return nil, p.errf("expected ':' after object key")
		}
		p.pos++
		p.skipSpace()
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		n.obj = append(n.obj, member{key: key, val: v})
		p.skipSpace()
		if p.pos >= len(p.data) {
			return nil, p.errf("unexpected end of object")
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
		case '}':
			p.pos++
			return n, nil
		default:
			return nil, p.errf("expected ',' or '}' in object")
		}
	}
}

func (p *parser) array() (*node, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer func() { p.depth-- }()
	p.pos++ // '['
	n := &node{kind: kArray}
	p.skipSpace()
	if p.pos < len(p.data) && p.data[p.pos] == ']' {
		p.pos++
		return n, nil
	}
	for {
		p.skipSpace()
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		n.arr = append(n.arr, v)
		p.skipSpace()
		if p.pos >= len(p.data) {
			return nil, p.errf("unexpected end of array")
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
		case ']':
			p.pos++
			return n, nil
		default:
			return nil, p.errf("expected ',' or ']' in array")
		}
	}
}

func (p *parser) string() (string, error) {
	p.pos++ // opening quote
	var sb strings.Builder
	for {
		if p.pos >= len(p.data) {
			return "", p.errf("unterminated string")
		}
		c := p.data[p.pos]
		switch {
		case c == '"':
			p.pos++
			return sb.String(), nil
		case c == '\\':
			if err := p.escape(&sb); err != nil {
				return "", err
			}
		case c < 0x20:
			return "", p.errf("unescaped control character in string")
		default:
			// 输入已整体校验为合法 UTF-8，这里按字节原样复制。
			sb.WriteByte(c)
			p.pos++
		}
	}
}

func (p *parser) escape(sb *strings.Builder) error {
	if p.pos+1 >= len(p.data) {
		return p.errf("unterminated escape")
	}
	c := p.data[p.pos+1]
	switch c {
	case '"', '\\', '/':
		sb.WriteByte(c)
		p.pos += 2
		return nil
	case 'b':
		sb.WriteByte('\b')
	case 'f':
		sb.WriteByte('\f')
	case 'n':
		sb.WriteByte('\n')
	case 'r':
		sb.WriteByte('\r')
	case 't':
		sb.WriteByte('\t')
	case 'u':
		r, err := p.hex4(p.pos + 2)
		if err != nil {
			return err
		}
		p.pos += 6
		if utf16.IsSurrogate(r) {
			if r >= 0xDC00 {
				return p.errf("lone low surrogate in string")
			}
			if p.pos+6 > len(p.data) || p.data[p.pos] != '\\' || p.data[p.pos+1] != 'u' {
				return p.errf("lone high surrogate in string")
			}
			r2, err := p.hex4(p.pos + 2)
			if err != nil {
				return err
			}
			if r2 < 0xDC00 || r2 > 0xDFFF {
				return p.errf("invalid surrogate pair in string")
			}
			p.pos += 6
			r = utf16.DecodeRune(r, r2)
		}
		sb.WriteRune(r)
		return nil
	default:
		return p.errf("invalid escape character %q", c)
	}
	p.pos += 2
	return nil
}

func (p *parser) hex4(at int) (rune, error) {
	if at+4 > len(p.data) {
		return 0, p.errf("truncated \\u escape")
	}
	v, err := strconv.ParseUint(string(p.data[at:at+4]), 16, 32)
	if err != nil {
		return 0, p.errf("invalid \\u escape")
	}
	return rune(v), nil
}

func (p *parser) number() (*node, error) {
	start := p.pos
	if p.data[p.pos] == '-' {
		p.pos++
	}
	if p.pos >= len(p.data) {
		return nil, p.errf("truncated number")
	}
	switch c := p.data[p.pos]; {
	case c == '0':
		p.pos++
	case c >= '1' && c <= '9':
		for p.pos < len(p.data) && isDigit(p.data[p.pos]) {
			p.pos++
		}
	default:
		return nil, p.errf("invalid number")
	}
	integer := true
	if p.pos < len(p.data) && p.data[p.pos] == '.' {
		integer = false
		p.pos++
		if p.pos >= len(p.data) || !isDigit(p.data[p.pos]) {
			return nil, p.errf("invalid number fraction")
		}
		for p.pos < len(p.data) && isDigit(p.data[p.pos]) {
			p.pos++
		}
	}
	if p.pos < len(p.data) && (p.data[p.pos] == 'e' || p.data[p.pos] == 'E') {
		integer = false
		p.pos++
		if p.pos < len(p.data) && (p.data[p.pos] == '+' || p.data[p.pos] == '-') {
			p.pos++
		}
		if p.pos >= len(p.data) || !isDigit(p.data[p.pos]) {
			return nil, p.errf("invalid number exponent")
		}
		for p.pos < len(p.data) && isDigit(p.data[p.pos]) {
			p.pos++
		}
	}
	lit := string(p.data[start:p.pos])
	f, err := strconv.ParseFloat(lit, 64)
	if err != nil || math.IsInf(f, 0) {
		return nil, &Error{Offset: start, Reason: "number out of IEEE-754 double range"}
	}
	if integer && math.Abs(f) > MaxSafeInteger {
		return nil, &Error{Offset: start, Reason: "integer literal outside ±(2^53-1)"}
	}
	return &node{kind: kNumber, num: f, lit: lit}, nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func writeNode(buf *bytes.Buffer, n *node) {
	switch n.kind {
	case kNull:
		buf.WriteString("null")
	case kBool:
		if n.b {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case kNumber:
		buf.WriteString(FormatNumber(n.num))
	case kString:
		writeString(buf, n.str)
	case kArray:
		buf.WriteByte('[')
		for i, c := range n.arr {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeNode(buf, c)
		}
		buf.WriteByte(']')
	case kObject:
		members := slices.Clone(n.obj)
		keys := make(map[string][]uint16, len(members))
		for _, m := range members {
			keys[m.key] = utf16.Encode([]rune(m.key))
		}
		slices.SortFunc(members, func(a, b member) int {
			return slices.Compare(keys[a.key], keys[b.key])
		})
		buf.WriteByte('{')
		for i, m := range members {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeString(buf, m.key)
			buf.WriteByte(':')
			writeNode(buf, m.val)
		}
		buf.WriteByte('}')
	}
}

const hexDigits = "0123456789abcdef"

func writeString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			if c < 0x20 {
				buf.WriteString(`\u00`)
				buf.WriteByte(hexDigits[c>>4])
				buf.WriteByte(hexDigits[c&0xF])
			} else {
				buf.WriteByte(c)
			}
		}
	}
	buf.WriteByte('"')
}

// FormatNumber 按 ECMAScript Number::toString 输出双精度数，
// 与 RFC 8785 §3.2.2.3 一致；NaN/Inf 不会出现在合法 JSON 中，调用方负责排除。
func FormatNumber(f float64) string {
	if f == 0 {
		return "0" // 包括 -0
	}
	if f < 0 {
		return "-" + FormatNumber(-f)
	}
	// 最短可往返的十进制表示：d.ddddde±XX。
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mant, expPart, _ := strings.Cut(e, "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp, _ := strconv.Atoi(expPart)
	k := len(digits)
	n := exp + 1 // 值 = 0.digits × 10^n
	var sb strings.Builder
	switch {
	case k <= n && n <= 21:
		sb.WriteString(digits)
		sb.WriteString(strings.Repeat("0", n-k))
	case 0 < n && n <= 21:
		sb.WriteString(digits[:n])
		sb.WriteByte('.')
		sb.WriteString(digits[n:])
	case -6 < n && n <= 0:
		sb.WriteString("0.")
		sb.WriteString(strings.Repeat("0", -n))
		sb.WriteString(digits)
	default:
		sb.WriteByte(digits[0])
		if k > 1 {
			sb.WriteByte('.')
			sb.WriteString(digits[1:])
		}
		sb.WriteByte('e')
		if n-1 >= 0 {
			sb.WriteByte('+')
		} else {
			sb.WriteByte('-')
		}
		sb.WriteString(strconv.Itoa(abs(n - 1)))
	}
	return sb.String()
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
