// Package schema 加载嵌入的权威 JSON Schema 并校验文档。
//
// 所有 schema 都从 schemas.FS 读取并以其 $id 注册，引用解析不访问网络；
// 未登记的外部 $ref 在编译时失败。校验输入应来自 canonjson.Decode 或
// yamljson.Decode，以保证数字、重复键等按统一规则解释。schema 校验只检查
// 结构，不替代授权、幂等与领域规则。
package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/yamljson"
	"github.com/oujinhaoai/lantai/schemas"
)

// Kind 区分可独立校验的文档 schema 与只供引用的定义库。
type Kind string

const (
	KindDocument Kind = "document"
	KindLibrary  Kind = "library"
)

// Entry 是 schemas/index.json 中的一项。
type Entry struct {
	Contract string `json:"contract"`
	Path     string `json:"path"`
	Kind     Kind   `json:"kind"`
	Owner    string `json:"owner"`
}

// URI 返回该 schema 的 $id。
func (e Entry) URI() string { return schemas.BaseURI + e.Path }

type index struct {
	Contract string  `json:"contract"`
	BaseURI  string  `json:"base_uri"`
	Schemas  []Entry `json:"schemas"`
}

// Registry 持有已编译的 schema，可并发使用。
type Registry struct {
	entries  []Entry
	byKey    map[string]Entry
	compiled map[string]*jsonschema.Schema // 文档 schema，Load 后只读

	mu        sync.Mutex // 保护 compiler 与 fragments
	compiler  *jsonschema.Compiler
	fragments map[string]*jsonschema.Schema
}

// Issue 是一条结构校验失败。
type Issue struct {
	// Pointer 是实例中的 JSON Pointer（RFC 6901）。
	Pointer string `json:"pointer"`
	// Keyword 是触发失败的 schema 关键字，例如 required、pattern。
	Keyword string `json:"keyword"`
	Message string `json:"message"`
}

// ValidationError 汇总一次校验的全部失败，对应错误码 SCHEMA_INVALID。
type ValidationError struct {
	Contract string
	Issues   []Issue
}

func (e *ValidationError) Error() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "schema: document does not satisfy %s", e.Contract)
	for i, is := range e.Issues {
		if i == 5 {
			fmt.Fprintf(&sb, "; and %d more", len(e.Issues)-5)
			break
		}
		fmt.Fprintf(&sb, "; %s: %s (%s)", displayPointer(is.Pointer), is.Message, is.Keyword)
	}
	return sb.String()
}

// Err 把校验失败转换为 SCHEMA_INVALID 结构化错误，逐项列出位置与原因。
func (e *ValidationError) Err() *errcode.Error {
	out := errcode.Newf(errcode.SchemaInvalid, "document does not satisfy %s", e.Contract)
	for i, is := range e.Issues {
		if i == 100 {
			break // 与错误信封 details 的上限一致
		}
		out.WithDetails(errcode.PointerDetail(snakeCase(is.Keyword), is.Pointer, is.Message))
	}
	return out
}

// snakeCase 把 schema 关键字（如 additionalProperties）转换为 details.reason 要求的小写下划线形式。
func snakeCase(s string) string {
	var sb strings.Builder
	for i, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			if i > 0 {
				sb.WriteByte('_')
			}
			sb.WriteRune(r + ('a' - 'A'))
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_':
			sb.WriteRune(r)
		}
	}
	if sb.Len() == 0 || sb.String()[0] < 'a' || sb.String()[0] > 'z' {
		return "schema"
	}
	return sb.String()
}

func displayPointer(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

// ErrUnknownSchema 表示给定的契约标识或 URI 未登记。
var ErrUnknownSchema = errors.New("schema: unknown contract or schema URI")

var (
	defaultOnce sync.Once
	defaultReg  *Registry
	defaultErr  error
)

// Default 返回从嵌入文件加载的共享注册表；首次调用时编译。
func Default() (*Registry, error) {
	defaultOnce.Do(func() { defaultReg, defaultErr = Load(schemas.FS) })
	return defaultReg, defaultErr
}

// Load 从 fsys 读取 index.json 并编译全部 schema。
func Load(fsys fs.FS) (*Registry, error) {
	raw, err := fs.ReadFile(fsys, "index.json")
	if err != nil {
		return nil, fmt.Errorf("schema: read index: %w", err)
	}
	var idx index
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("schema: parse index: %w", err)
	}
	if idx.BaseURI != schemas.BaseURI {
		return nil, fmt.Errorf("schema: index base_uri %q does not match %q", idx.BaseURI, schemas.BaseURI)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	c.UseLoader(noNetworkLoader{})

	r := &Registry{
		byKey:     map[string]Entry{},
		compiled:  map[string]*jsonschema.Schema{},
		compiler:  c,
		fragments: map[string]*jsonschema.Schema{},
	}
	for _, e := range idx.Schemas {
		if e.Kind != KindDocument && e.Kind != KindLibrary {
			return nil, fmt.Errorf("schema: %s: unknown kind %q", e.Path, e.Kind)
		}
		if _, dup := r.byKey[e.Contract]; dup {
			return nil, fmt.Errorf("schema: duplicate contract %s", e.Contract)
		}
		data, err := fs.ReadFile(fsys, e.Path)
		if err != nil {
			return nil, fmt.Errorf("schema: read %s: %w", e.Path, err)
		}
		doc, err := canonjson.Decode(data)
		if err != nil {
			return nil, fmt.Errorf("schema: %s: %w", e.Path, err)
		}
		obj, ok := doc.(map[string]any)
		if !ok || obj["$id"] != e.URI() {
			return nil, fmt.Errorf("schema: %s: $id must be %s", e.Path, e.URI())
		}
		if err := c.AddResource(e.URI(), doc); err != nil {
			return nil, fmt.Errorf("schema: add %s: %w", e.Path, err)
		}
		r.entries = append(r.entries, e)
		r.byKey[e.Contract] = e
		r.byKey[e.URI()] = e
	}
	for _, e := range r.entries {
		if e.Kind != KindDocument {
			continue
		}
		sch, err := c.Compile(e.URI())
		if err != nil {
			return nil, fmt.Errorf("schema: compile %s: %w", e.Path, err)
		}
		r.compiled[e.Contract] = sch
	}
	// 定义库本身也要能编译通过（包括其内部引用）。
	for _, e := range r.entries {
		if e.Kind != KindLibrary {
			continue
		}
		if _, err := c.Compile(e.URI()); err != nil {
			return nil, fmt.Errorf("schema: compile %s: %w", e.Path, err)
		}
	}
	return r, nil
}

// noNetworkLoader 拒绝加载任何未登记的资源，保证不访问网络或本机任意文件。
type noNetworkLoader struct{}

func (noNetworkLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("schema: refusing to load unregistered resource %s", url)
}

// DefPattern 编译公共定义库 lantai.common-defs/v1 中某个定义的 pattern，
// 让 Go 代码复用 schema 中的同一条规则而不另写一份正则。
func DefPattern(name string) (*regexp.Regexp, error) {
	raw, err := fs.ReadFile(schemas.FS, "common/v1/defs.schema.json")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Defs map[string]struct {
			Pattern string `json:"pattern"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	d, ok := doc.Defs[name]
	if !ok || d.Pattern == "" {
		return nil, fmt.Errorf("schema: common definition %q has no pattern", name)
	}
	return regexp.Compile(d.Pattern)
}

// MustDefPattern 与 DefPattern 相同，失败时 panic（用于包级变量）。
func MustDefPattern(name string) *regexp.Regexp {
	re, err := DefPattern(name)
	if err != nil {
		panic(err)
	}
	return re
}

// Entries 返回按契约标识排序的登记项。
func (r *Registry) Entries() []Entry {
	out := slices.Clone(r.entries)
	sort.Slice(out, func(i, j int) bool { return out[i].Contract < out[j].Contract })
	return out
}

// Lookup 按契约标识或 $id 查找登记项。
func (r *Registry) Lookup(key string) (Entry, bool) {
	e, ok := r.byKey[key]
	return e, ok
}

// Validate 校验已解码的文档；key 为契约标识（如 lantai.error/v1）或 $id。
// 片段形式 "<contract>#/$defs/<name>" 可校验定义库中的单个定义。
func (r *Registry) Validate(key string, doc any) error {
	sch, contract, err := r.resolve(key)
	if err != nil {
		return err
	}
	if err := sch.Validate(doc); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			return &ValidationError{Contract: contract, Issues: issuesOf(ve)}
		}
		return fmt.Errorf("schema: validate %s: %w", contract, err)
	}
	return nil
}

// Check 确认 key 能解析为可校验的 schema（契约标识、$id 或带 JSON Pointer
// 片段的定义），但不校验任何文档；解析失败返回 ErrUnknownSchema。
func (r *Registry) Check(key string) error {
	_, _, err := r.resolve(key)
	return err
}

// ValidateJSON 严格解析 JSON 后校验。
func (r *Registry) ValidateJSON(key string, data []byte) error {
	doc, err := canonjson.Decode(data)
	if err != nil {
		return err
	}
	return r.Validate(key, doc)
}

// ValidateYAML 按 JSON 数据模型解析 YAML 后校验。
func (r *Registry) ValidateYAML(key string, data []byte) error {
	doc, err := yamljson.Decode(data)
	if err != nil {
		return err
	}
	return r.Validate(key, doc)
}

func (r *Registry) resolve(key string) (*jsonschema.Schema, string, error) {
	base, frag, hasFrag := strings.Cut(key, "#")
	e, ok := r.byKey[base]
	if !ok {
		return nil, "", fmt.Errorf("%w: %s", ErrUnknownSchema, key)
	}
	if !hasFrag {
		if e.Kind != KindDocument {
			return nil, "", fmt.Errorf("%w: %s is a definition library; validate a fragment like %s#/$defs/<name>", ErrUnknownSchema, e.Contract, e.Contract)
		}
		return r.compiled[e.Contract], e.Contract, nil
	}
	if !strings.HasPrefix(frag, "/") {
		return nil, "", fmt.Errorf("%w: fragment of %s must be a JSON pointer such as #/$defs/<name>", ErrUnknownSchema, key)
	}
	// 片段按需编译；定义库里的定义通过同一编译器解析引用。
	sch, err := r.compileFragment(e, frag)
	if err != nil {
		return nil, "", err
	}
	return sch, e.Contract + "#" + frag, nil
}

func (r *Registry) compileFragment(e Entry, frag string) (*jsonschema.Schema, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := e.Contract + "#" + frag
	if sch, ok := r.fragments[key]; ok {
		return sch, nil
	}
	sch, err := r.compiler.Compile(e.URI() + "#" + frag)
	if err != nil {
		return nil, fmt.Errorf("%w: compile %s: %v", ErrUnknownSchema, key, err)
	}
	r.fragments[key] = sch
	return sch, nil
}

var printer = message.NewPrinter(language.English)

// issuesOf 收集校验失败树的叶子节点；组合关键字（allOf、$ref、then 等）
// 只作为路径，不单独列出。
func issuesOf(ve *jsonschema.ValidationError) []Issue {
	var issues []Issue
	seen := map[Issue]bool{}
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) > 0 {
			for _, c := range e.Causes {
				walk(c)
			}
			return
		}
		kw := keywordOf(e)
		is := Issue{
			Pointer: joinPointer(e.InstanceLocation),
			Keyword: kw,
			Message: e.ErrorKind.LocalizedString(printer),
		}
		if !seen[is] {
			seen[is] = true
			issues = append(issues, is)
		}
	}
	walk(ve)
	sort.SliceStable(issues, func(i, j int) bool { return issues[i].Pointer < issues[j].Pointer })
	return issues
}

// keywordOf 返回失败的 schema 关键字。校验库的 not、false schema 错误
// 不带关键字路径，这里显式映射。
func keywordOf(e *jsonschema.ValidationError) string {
	switch e.ErrorKind.(type) {
	case *kind.Not:
		return "not"
	case *kind.FalseSchema:
		return "false"
	}
	if kp := e.ErrorKind.KeywordPath(); len(kp) > 0 {
		return kp[0]
	}
	return "schema"
}

func joinPointer(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}
	escaped := make([]string, len(tokens))
	for i, t := range tokens {
		escaped[i] = strings.ReplaceAll(strings.ReplaceAll(t, "~", "~0"), "/", "~1")
	}
	return "/" + strings.Join(escaped, "/")
}
