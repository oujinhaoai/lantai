// openapi 把 api/openapi.yaml 与其引用的 schemas/ 定义打包为自包含文档
// api/gen/openapi.bundle.json，供代码生成器与外部 SDK 工具使用。
//
// 外部 $ref 被内联到 components/schemas：公共定义库中的定义按定义名命名
// （ulid → Ulid），其他文档按其 title 命名，文档内定义命名为 title+定义名。
// 只导入被引用到的定义；命名冲突或无法解析的引用直接失败。
//
//	go run ./scripts/gen/openapi          # 写入
//	go run ./scripts/gen/openapi -check   # 只核对
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/yamljson"
	"github.com/oujinhaoai/lantai/schemas"
	"github.com/oujinhaoai/lantai/scripts/gen/internal/genfile"
)

const (
	source = "api/openapi.yaml"
	output = "api/gen/openapi.bundle.json"
	// commonDefs 中的定义直接以定义名命名。
	commonDefs = "common/v1/defs.schema.json"
)

func main() {
	check := flag.Bool("check", false, "compare generated output with the file on disk instead of writing")
	flag.Parse()
	if err := run(*check); err != nil {
		fmt.Fprintln(os.Stderr, "openapi:", err)
		os.Exit(1)
	}
}

func run(check bool) error {
	root, err := genfile.Root()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(root, source))
	if err != nil {
		return err
	}
	out, err := Bundle(raw)
	if err != nil {
		return err
	}
	return genfile.Output(filepath.Join(root, output), out, check)
}

type target struct {
	file string // 相对 schemas/ 的路径
	frag string // 形如 /$defs/ulid，空表示整个文档
}

type bundler struct {
	docs    map[string]map[string]any
	names   map[target]string
	byName  map[string]target
	pending []target
	out     map[string]any
}

// Bundle 返回打包后的规范 JSON（带缩进）。
func Bundle(openapiYAML []byte) ([]byte, error) {
	v, err := yamljson.Decode(openapiYAML)
	if err != nil {
		return nil, err
	}
	doc, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s is not an object", source)
	}
	b := &bundler{docs: map[string]map[string]any{}, names: map[target]string{}, byName: map[string]target{}, out: map[string]any{}}
	// api/ 下的相对引用以 api/ 为基准解析。
	rewritten, err := b.rewrite(doc, "api", true)
	if err != nil {
		return nil, err
	}
	doc = rewritten.(map[string]any)
	for len(b.pending) > 0 {
		t := b.pending[0]
		b.pending = b.pending[1:]
		sub, err := b.resolve(t)
		if err != nil {
			return nil, err
		}
		sub = stripSchemaKeywords(deepCopy(sub))
		conv, err := b.rewrite(sub, path.Join("schemas", path.Dir(t.file)), false, t)
		if err != nil {
			return nil, err
		}
		b.out[b.names[t]] = conv
	}
	comps, _ := doc["components"].(map[string]any)
	if comps == nil {
		comps = map[string]any{}
		doc["components"] = comps
	}
	existing, _ := comps["schemas"].(map[string]any)
	if existing == nil {
		existing = map[string]any{}
	}
	for name, s := range b.out {
		if _, clash := existing[name]; clash {
			return nil, fmt.Errorf("component %s already defined in %s", name, source)
		}
		existing[name] = s
	}
	comps["schemas"] = existing
	if err := checkRefs(doc, doc); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	canonical, err := canonjson.Canonicalize(raw)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, canonical, "", "  "); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// rewrite 深度遍历，把外部 $ref 换成 components 引用；base 为当前文件所在目录（相对仓库根）。
// ctx 非空时表示正在导入某个 schema 文档，文档内的 #/$defs/x 引用解析到该文档。
func (b *bundler) rewrite(v any, base string, isOpenAPI bool, ctx ...target) (any, error) {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if k == "$ref" {
				ref, ok := val.(string)
				if !ok {
					return nil, fmt.Errorf("$ref must be a string")
				}
				nr, err := b.mapRef(ref, base, isOpenAPI, ctx...)
				if err != nil {
					return nil, err
				}
				out[k] = nr
				continue
			}
			nv, err := b.rewrite(val, base, isOpenAPI, ctx...)
			if err != nil {
				return nil, err
			}
			out[k] = nv
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			nv, err := b.rewrite(val, base, isOpenAPI, ctx...)
			if err != nil {
				return nil, err
			}
			out[i] = nv
		}
		return out, nil
	default:
		return v, nil
	}
}

func (b *bundler) mapRef(ref, base string, isOpenAPI bool, ctx ...target) (string, error) {
	file, frag, _ := strings.Cut(ref, "#")
	if file == "" {
		if isOpenAPI {
			return ref, nil // OpenAPI 文档内部引用保持不变
		}
		// schema 文档内部引用：#/$defs/x → 同一文件的定义。
		return b.enqueue(target{file: ctx[0].file, frag: frag})
	}
	if local, ok := strings.CutPrefix(file, schemas.BaseURI); ok {
		if path.Clean(local) != local || strings.HasPrefix(local, "/") {
			return "", fmt.Errorf("invalid local schema reference %q", ref)
		}
		return b.enqueue(target{file: local, frag: frag})
	}
	if strings.Contains(file, "://") {
		return "", fmt.Errorf("remote reference %q is not allowed", ref)
	}
	repoPath := path.Clean(path.Join(base, file))
	rel, ok := strings.CutPrefix(repoPath, "schemas/")
	if !ok {
		return "", fmt.Errorf("reference %q resolves outside schemas/ (%s)", ref, repoPath)
	}
	return b.enqueue(target{file: rel, frag: frag})
}

func (b *bundler) enqueue(t target) (string, error) {
	if name, ok := b.names[t]; ok {
		return "#/components/schemas/" + name, nil
	}
	name, err := b.nameFor(t)
	if err != nil {
		return "", err
	}
	if other, clash := b.byName[name]; clash && other != t {
		return "", fmt.Errorf("component name %s is used by both %v and %v", name, other, t)
	}
	b.names[t], b.byName[name] = name, t
	b.pending = append(b.pending, t)
	return "#/components/schemas/" + name, nil
}

func (b *bundler) load(file string) (map[string]any, error) {
	if d, ok := b.docs[file]; ok {
		return d, nil
	}
	raw, err := fs.ReadFile(schemas.FS, file)
	if err != nil {
		return nil, err
	}
	v, err := canonjson.Decode(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	d, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s is not an object", file)
	}
	b.docs[file] = d
	return d, nil
}

func (b *bundler) resolve(t target) (any, error) {
	d, err := b.load(t.file)
	if err != nil {
		return nil, err
	}
	var cur any = d
	if t.frag == "" {
		return cur, nil
	}
	for _, tok := range strings.Split(strings.TrimPrefix(t.frag, "/"), "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("cannot resolve %s#%s", t.file, t.frag)
		}
		if cur, ok = m[tok]; !ok {
			return nil, fmt.Errorf("cannot resolve %s#%s", t.file, t.frag)
		}
	}
	return cur, nil
}

func (b *bundler) nameFor(t target) (string, error) {
	defName := ""
	if t.frag != "" {
		name, ok := strings.CutPrefix(t.frag, "/$defs/")
		if !ok || strings.Contains(name, "/") {
			return "", fmt.Errorf("only whole documents and top-level $defs can be referenced, got %s#%s", t.file, t.frag)
		}
		defName = pascal(name)
	}
	if t.file == commonDefs {
		if defName == "" {
			return "", fmt.Errorf("%s is a library; reference a definition", commonDefs)
		}
		return defName, nil
	}
	d, err := b.load(t.file)
	if err != nil {
		return "", err
	}
	title, _ := d["title"].(string)
	// Versioned execution contracts retain their authoritative protocol titles.
	if strings.HasPrefix(title, "lantai.") {
		title = pascal(strings.NewReplacer(".", "_", "-", "_", "/", "_").Replace(title))
	}
	if strings.ContainsAny(title, " -./") {
		title = pascal(strings.NewReplacer("/", "_", "-", "_", ".", "_", " ", "_").Replace(strings.TrimSuffix(t.file, ".schema.json")))
	}
	if title == "" {
		return "", fmt.Errorf("%s needs a PascalCase title to be bundled", t.file)
	}
	return title + defName, nil
}

func pascal(s string) string {
	var sb strings.Builder
	for _, part := range strings.Split(s, "_") {
		if part == "" {
			continue
		}
		sb.WriteString(strings.ToUpper(part[:1]))
		sb.WriteString(part[1:])
	}
	return sb.String()
}

func stripSchemaKeywords(v any) any {
	if m, ok := v.(map[string]any); ok {
		delete(m, "$schema")
		delete(m, "$id")
		delete(m, "$defs") // 被引用的定义单独导入为组件
	}
	return v
}

func deepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = deepCopy(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = deepCopy(val)
		}
		return out
	default:
		return v
	}
}

// checkRefs 确认打包结果中的每个 $ref 都是可解析的内部引用。
func checkRefs(root map[string]any, v any) error {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if k == "$ref" {
				ref, _ := x[k].(string)
				if !strings.HasPrefix(ref, "#/") {
					return fmt.Errorf("unbundled reference %q", ref)
				}
				if _, err := pointer(root, strings.TrimPrefix(ref, "#")); err != nil {
					return fmt.Errorf("dangling reference %q", ref)
				}
				continue
			}
			if err := checkRefs(root, x[k]); err != nil {
				return err
			}
		}
	case []any:
		for _, val := range x {
			if err := checkRefs(root, val); err != nil {
				return err
			}
		}
	}
	return nil
}

func pointer(root any, p string) (any, error) {
	cur := root
	for _, tok := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("not an object at %s", tok)
		}
		if cur, ok = m[tok]; !ok {
			return nil, fmt.Errorf("missing %s", tok)
		}
	}
	return cur, nil
}
