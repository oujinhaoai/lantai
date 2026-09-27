// Package pathrule 实现兰台跨平台的路径与名称规范（T02.1）。
//
// 数据目录要能在 Linux、macOS、Windows 之间搬移，版本内的文件路径与资产
// 路径别名（slug）在入藏时统一规范化：用 / 分隔，存成 Unicode NFC；禁用
// Windows 建不出来的字符、保留设备名以及结尾的点和空格；限制长度；拒绝
// 空段、. 与 .. 段、绝对路径、反斜杠、控制字符与双向文字控制符。同一集合
// 里规范化后只差大小写或 Unicode 写法的两个路径、以及一个路径同时作文件
// 和目录，一律返回 PATH_CONFLICT，不静默改名。
//
// 冲突判定用折叠键（NFC → Unicode 完全大小写折叠 → NFC）。它比各文件系统
// 的实际比较规则更保守：宁可拒绝极少数在某个系统上本可共存的名字，也不
// 让它们在另一个系统上互相覆盖。
//
// 本包只做纯字符串判定，由 catalog 冻结清单、storage 写盘前与 ledger 比较
// 占名时共同使用，保证各处规则一致。
package pathrule

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

// 长度上限。路径按 Unicode 码点计，与 schemas 中 relative_path 与
// normalized_slug 的 maxLength 一致；单段按 UTF-8 字节计，取常见文件系统
// 文件名上限（255 字节）作保守值。
const (
	MaxPathRunes    = 200
	MaxSegmentBytes = 255
)

// 错误细节中的原因（小写下划线，供机器判断）。
const (
	ReasonInvalidUTF8        = "invalid_utf8"
	ReasonEmpty              = "empty_path"
	ReasonAbsolute           = "absolute_path"
	ReasonTrailingSlash      = "trailing_slash"
	ReasonEmptySegment       = "empty_segment"
	ReasonDotSegment         = "dot_segment"
	ReasonBackslash          = "backslash"
	ReasonControlCharacter   = "control_character"
	ReasonBidiControl        = "bidi_control"
	ReasonReservedCharacter  = "reserved_character"
	ReasonReservedName       = "reserved_name"
	ReasonTrailingDotOrSpace = "trailing_dot_or_space"
	ReasonPathTooLong        = "path_too_long"
	ReasonSegmentTooLong     = "segment_too_long"
	ReasonNotNormalized      = "not_normalized"
	ReasonAtSign             = "at_sign"
	ReasonSameName           = "same_name_after_normalization"
	ReasonFileDirectory      = "file_directory_conflict"
	ReasonInvalidProjectKey  = "invalid_project_key"
)

// windowsReserved 是 Windows 文件名中不允许出现的可见字符（反斜杠单独报告）。
const windowsReserved = `<>:"|?*`

// reservedNames 是 Windows 的保留设备名（不区分大小写，带任何扩展名也保留）。
var reservedNames = func() map[string]bool {
	m := map[string]bool{"con": true, "prn": true, "aux": true, "nul": true, "conin$": true, "conout$": true}
	for _, p := range []string{"com", "lpt"} {
		for _, d := range []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³"} {
			m[p+d] = true
		}
	}
	return m
}()

// bidiControls 是会让显示顺序与存储顺序不一致的双向文字控制符，可被用来
// 伪装扩展名；一律拒绝。
func bidiControl(r rune) bool {
	switch {
	case r == 0x061C, r == 0x200E, r == 0x200F:
		return true
	case r >= 0x202A && r <= 0x202E:
		return true
	case r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}

func invalid(reason, input, message string) *errcode.Error {
	return errcode.New(errcode.SchemaInvalid, message).
		WithDetails(errcode.Detail{Reason: reason, Message: message, Data: map[string]any{"path": input}})
}

// Normalize 把相对路径规范化为 NFC 并校验跨平台规则，返回规范形式。
// 不合法时返回 SCHEMA_INVALID，details 给出原因。
func Normalize(p string) (string, error) {
	if !utf8.ValidString(p) {
		return "", invalid(ReasonInvalidUTF8, "", "path is not valid UTF-8")
	}
	n := norm.NFC.String(p)
	if err := check(n, p); err != nil {
		return "", err
	}
	return n, nil
}

// Check 确认 p 已是规范形式（NFC）且合法；用于核对已冻结的清单与写盘前的防御检查。
func Check(p string) error {
	if !utf8.ValidString(p) {
		return invalid(ReasonInvalidUTF8, "", "path is not valid UTF-8")
	}
	if !norm.NFC.IsNormalString(p) {
		return invalid(ReasonNotNormalized, p, "path is not in Unicode NFC form")
	}
	return check(p, p)
}

func check(n, input string) error {
	switch {
	case n == "":
		return invalid(ReasonEmpty, input, "path is empty")
	case strings.HasPrefix(n, "/"):
		return invalid(ReasonAbsolute, input, "path must be relative")
	case strings.HasSuffix(n, "/"):
		return invalid(ReasonTrailingSlash, input, "path must not end with /")
	}
	if utf8.RuneCountInString(n) > MaxPathRunes {
		return invalid(ReasonPathTooLong, input, fmt.Sprintf("path is longer than %d characters", MaxPathRunes))
	}
	for _, seg := range strings.Split(n, "/") {
		if err := checkSegment(seg, input); err != nil {
			return err
		}
	}
	return nil
}

func checkSegment(seg, input string) error {
	switch {
	case seg == "":
		return invalid(ReasonEmptySegment, input, "path contains an empty segment")
	case seg == "." || seg == "..":
		return invalid(ReasonDotSegment, input, "path must not contain . or .. segments")
	case len(seg) > MaxSegmentBytes:
		return invalid(ReasonSegmentTooLong, input, fmt.Sprintf("a path segment is longer than %d bytes", MaxSegmentBytes))
	}
	for _, r := range seg {
		switch {
		case r == '\\':
			return invalid(ReasonBackslash, input, "path must use / as separator, not \\")
		case r < 0x20 || r == 0x7F || unicode.Is(unicode.Cc, r):
			return invalid(ReasonControlCharacter, input, "path contains a control character")
		case bidiControl(r):
			return invalid(ReasonBidiControl, input, "path contains a bidirectional text control character")
		case strings.ContainsRune(windowsReserved, r):
			return invalid(ReasonReservedCharacter, input, fmt.Sprintf("path contains %q, which Windows does not allow", r))
		}
	}
	if last := seg[len(seg)-1]; last == '.' || last == ' ' {
		return invalid(ReasonTrailingDotOrSpace, input, "a path segment must not end with a dot or a space")
	}
	base := seg
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	if reservedNames[strings.ToLower(strings.TrimRight(base, " "))] {
		return invalid(ReasonReservedName, input, fmt.Sprintf("%q is a reserved device name on Windows", seg))
	}
	return nil
}

// Key 返回规范路径的冲突判定键：NFC → Unicode 完全大小写折叠 → NFC。
// 两个路径的键相同即视为同名。输入应已规范化。
func Key(p string) string {
	// cases.Caser 带状态，不能跨 goroutine 共用，每次新建。
	return norm.NFC.String(cases.Fold().String(norm.NFC.String(p)))
}

// Conflict 描述集合中的一处冲突。
type Conflict struct {
	Reason string
	A, B   string
}

// Conflicts 列出集合中规范化后同名（只差大小写或 Unicode 写法）的路径，以及
// 一个路径同时被当作文件与另一路径的上级目录的情形。输入应已规范化。
func Conflicts(paths []string) []Conflict {
	var out []Conflict
	keys := make(map[string]string, len(paths))
	for _, p := range paths {
		k := Key(p)
		if prev, ok := keys[k]; ok {
			out = append(out, Conflict{Reason: ReasonSameName, A: prev, B: p})
			continue
		}
		keys[k] = p
	}
	for _, p := range paths {
		k := Key(p)
		for i := 0; i < len(k); i++ {
			if k[i] != '/' {
				continue
			}
			if file, ok := keys[k[:i]]; ok {
				out = append(out, Conflict{Reason: ReasonFileDirectory, A: file, B: p})
			}
		}
	}
	return out
}

// CheckSet 在集合有冲突时返回 PATH_CONFLICT，details 逐项列出。
func CheckSet(paths []string) error {
	cs := Conflicts(paths)
	if len(cs) == 0 {
		return nil
	}
	e := errcode.New(errcode.PathConflict, "paths conflict after normalization").
		WithHint("rename one of the paths; the server does not rename silently")
	for _, c := range cs {
		msg := "the paths differ only in case or Unicode form"
		if c.Reason == ReasonFileDirectory {
			msg = "a path is used both as a file and as a directory"
		}
		e.WithDetails(errcode.Detail{Reason: c.Reason, Message: msg, Data: map[string]any{"paths": []string{c.A, c.B}}})
	}
	return e
}

// NormalizeSlug 规范化项目内的资产路径别名：规则与文件路径相同，另外不允许
// "@"（引用语法用它分隔版本选择）。
func NormalizeSlug(s string) (string, error) {
	n, err := Normalize(s)
	if err != nil {
		return "", err
	}
	if strings.ContainsRune(n, '@') {
		return "", invalid(ReasonAtSign, s, `an asset path must not contain "@"`)
	}
	return n, nil
}

// CheckSlug 确认 s 已是规范的资产路径别名。
func CheckSlug(s string) error {
	if err := Check(s); err != nil {
		return err
	}
	if strings.ContainsRune(s, '@') {
		return invalid(ReasonAtSign, s, `an asset path must not contain "@"`)
	}
	return nil
}

var projectKeyRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// CheckProjectKey 校验项目 key：小写字母开头，只含小写字母、数字与连字符，
// 最长 63 个字符。key 是可读引用的第一段，建立后不可修改。
func CheckProjectKey(k string) error {
	if !projectKeyRE.MatchString(k) || strings.HasSuffix(k, "-") {
		return errcode.New(errcode.SchemaInvalid, "project key must match "+projectKeyRE.String()+" and not end with -").
			WithDetails(errcode.Detail{Reason: ReasonInvalidProjectKey, Data: map[string]any{"key": k}})
	}
	return nil
}
