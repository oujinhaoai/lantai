package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
)

const (
	MaxDependencyFileBytes  = 1 << 20
	MaxDependencyTotalBytes = 4 << 20
	MaxDependencyFiles      = 128
	DependencyReportSchema  = "lantai.selected-dependency-analysis/v1"
)

type DependencySelection struct {
	Paths          []string `json:"paths"`
	ComfyInputRoot string   `json:"comfy_input_root,omitempty"`
}
type DependencySource struct {
	Path  string
	Bytes []byte
}
type DependencyFileReport struct {
	Path       string   `json:"path"`
	SHA256     string   `json:"sha256"`
	Format     string   `json:"format"`
	References []string `json:"references"`
	Notices    []string `json:"notices"`
	Complete   bool     `json:"complete"`
}
type MissingDependency struct {
	From string `json:"from"`
	Path string `json:"path"`
}
type DependencyReport struct {
	Schema               string                 `json:"schema"`
	Scope                string                 `json:"scope"`
	Complete             bool                   `json:"complete"`
	Files                []DependencyFileReport `json:"files"`
	Dependencies         []WorksetDependency    `json:"dependencies"`
	MissingSelectedFiles []MissingDependency    `json:"missing_selected_files"`
	ComfyInputRoot       string                 `json:"comfy_input_root,omitempty"`
}

// AnalyzeDependencyFiles reads only explicitly selected regular files. It never
// scans for dependencies, opens a referenced path, fetches a URL or executes a
// project. CLI and MCP share this reader and the pure bounded parser below.
func AnalyzeDependencyFiles(ctx context.Context, in DependencySelection, directory string) (DependencyReport, error) {
	if len(in.Paths) < 1 || len(in.Paths) > MaxDependencyFiles {
		return DependencyReport{}, worksetInvalid("select1..128 dependency files")
	}
	if worksetConflicts(in.Paths) {
		return DependencyReport{}, worksetConflict("selected dependency paths conflict")
	}
	directory, err := importAbsolute(directory)
	if err != nil {
		return DependencyReport{}, err
	}
	root, err := openWorksetRoot(directory, false)
	if err != nil {
		return DependencyReport{}, err
	}
	if root == nil {
		return DependencyReport{}, os.ErrNotExist
	}
	defer root.Close()
	return analyzeDependencyRoot(ctx, in, root)
}

// AnalyzeDependencyWorkspace pins the selected directory beneath the caller's
// already pinned workspace. A renamed parent/symlink cannot redirect reads to
// private files outside that configured root.
func AnalyzeDependencyWorkspace(ctx context.Context, in DependencySelection, workspace *os.Root, directory string) (DependencyReport, error) {
	if workspace == nil {
		return DependencyReport{}, worksetInvalid("selected workspace required")
	}
	name := filepath.FromSlash(directory)
	if directory != "." {
		if err := worksetPath(directory); err != nil {
			return DependencyReport{}, err
		}
		if err := checkWorksetTarget(workspace, directory, true); err != nil {
			return DependencyReport{}, err
		}
	}
	root, err := workspace.OpenRoot(name)
	if err != nil {
		return DependencyReport{}, err
	}
	defer root.Close()
	selected, err := root.Stat(".")
	if err != nil {
		return DependencyReport{}, err
	}
	report, err := analyzeDependencyRoot(ctx, in, root)
	if err != nil {
		return report, err
	}
	if directory != "." {
		if err := checkWorksetTarget(workspace, directory, true); err != nil {
			return report, err
		}
	}
	current, err := workspace.Lstat(name)
	if err != nil || !current.IsDir() || !os.SameFile(selected, current) {
		return report, worksetConflict("selected dependency directory changed")
	}
	return report, nil
}
func analyzeDependencyRoot(ctx context.Context, in DependencySelection, root *os.Root) (DependencyReport, error) {
	if len(in.Paths) < 1 || len(in.Paths) > MaxDependencyFiles {
		return DependencyReport{}, worksetInvalid("select1..128 dependency files")
	}
	if worksetConflicts(in.Paths) {
		return DependencyReport{}, worksetConflict("selected dependency paths conflict")
	}
	sources := make([]DependencySource, 0, len(in.Paths))
	total := 0
	for _, name := range in.Paths {
		if err := ctx.Err(); err != nil {
			return DependencyReport{}, err
		}
		if err := worksetPath(name); err != nil {
			return DependencyReport{}, err
		}
		if err := checkWorksetTarget(root, name, false); err != nil {
			return DependencyReport{}, err
		}
		f, err := root.Open(filepath.FromSlash(name))
		if err != nil {
			return DependencyReport{}, err
		}
		before, err := f.Stat()
		if err != nil || !before.Mode().IsRegular() || before.Size() > MaxDependencyFileBytes {
			f.Close()
			return DependencyReport{}, worksetInvalid("selected dependency must be a regular file <=1MiB")
		}
		raw, err := io.ReadAll(io.LimitReader(f, MaxDependencyFileBytes+1))
		after, statErr := f.Stat()
		f.Close()
		if err != nil {
			return DependencyReport{}, err
		}
		entry, entryErr := root.Lstat(filepath.FromSlash(name))
		if statErr != nil || entryErr != nil || !os.SameFile(before, after) || !os.SameFile(after, entry) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || int64(len(raw)) != before.Size() {
			return DependencyReport{}, worksetConflict("selected dependency changed while reading")
		}
		total += len(raw)
		if len(raw) > MaxDependencyFileBytes || total > MaxDependencyTotalBytes {
			return DependencyReport{}, worksetInvalid("dependency limits:1MiB each,4MiB total")
		}
		sources = append(sources, DependencySource{Path: name, Bytes: raw})
	}
	return AnalyzeSelectedDependencies(ctx, sources, in.ComfyInputRoot)
}

func AnalyzeSelectedDependencies(ctx context.Context, sources []DependencySource, inputRoot string) (DependencyReport, error) {
	out := DependencyReport{Schema: DependencyReportSchema, Scope: "explicit-selected-text-files", Complete: true, Files: []DependencyFileReport{}, Dependencies: []WorksetDependency{}, MissingSelectedFiles: []MissingDependency{}, ComfyInputRoot: inputRoot}
	if len(sources) < 1 || len(sources) > MaxDependencyFiles {
		return out, worksetInvalid("select1..128 dependency files")
	}
	if inputRoot != "" && inputRoot != "." {
		if err := worksetPath(inputRoot); err != nil {
			return out, err
		}
	}
	paths := make([]string, 0, len(sources))
	selected := map[string]bool{}
	total := 0
	for _, source := range sources {
		if err := worksetPath(source.Path); err != nil {
			return out, err
		}
		if selected[source.Path] {
			return out, worksetConflict("duplicate selected dependency path")
		}
		selected[source.Path] = true
		paths = append(paths, source.Path)
		total += len(source.Bytes)
		if len(source.Bytes) > MaxDependencyFileBytes || total > MaxDependencyTotalBytes {
			return out, worksetInvalid("dependency limits:1MiB each,4MiB total")
		}
	}
	if worksetConflicts(paths) {
		return out, worksetConflict("selected dependency paths conflict")
	}
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		r := DependencyFileReport{Path: source.Path, SHA256: importSHA(source.Bytes), Format: "unsupported", References: []string{}, Notices: []string{}}
		err := parseDependencySource(ctx, source, inputRoot, &r)
		if cancelled := ctx.Err(); cancelled != nil {
			return out, cancelled
		}
		if err != nil {
			r.Notices = append(r.Notices, err.Error())
		}
		r.Complete = len(r.Notices) == 0
		out.Complete = out.Complete && r.Complete
		out.Files = append(out.Files, r)
		if len(r.References) > 0 {
			out.Dependencies = append(out.Dependencies, WorksetDependency{From: r.Path, Requires: r.References})
			for _, p := range r.References {
				if !selected[p] {
					out.MissingSelectedFiles = append(out.MissingSelectedFiles, MissingDependency{From: r.Path, Path: p})
				}
			}
		}
	}
	return out, nil
}

func parseDependencySource(ctx context.Context, source DependencySource, inputRoot string, out *DependencyFileReport) error {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(source.Path), "."))
	if ext != "obj" && ext != "mtl" && ext != "json" {
		return errors.New("不支持该格式；Blender/GLB/FBX/UE 二进制、脚本、模型和自定义节点不自动解析。")
	}
	if !utf8.Valid(source.Bytes) {
		return errors.New("文本不是合法 UTF-8。")
	}
	text := string(bytes.TrimPrefix(source.Bytes, []byte{0xef, 0xbb, 0xbf}))
	if strings.ContainsRune(text, 0) {
		return errors.New("包含二进制 NUL，不作为文本解析。")
	}
	if ext == "obj" || ext == "mtl" {
		out.Format = ext
		parsed := DependencyFileReport{References: []string{}, Notices: []string{}}
		if err := meshDependencyText(ctx, source.Path, text, ext, &parsed); err != nil {
			return err
		}
		out.References, out.Notices = parsed.References, parsed.Notices
		return nil
	}
	return comfyDependencyText(ctx, text, inputRoot, out)
}

func dependencyWords(line string) ([]string, error) {
	out := []string{}
	var value strings.Builder
	var quote rune
	for _, char := range line {
		if quote != 0 {
			if char == quote {
				quote = 0
			} else {
				value.WriteRune(char)
			}
		} else if char == '\'' || char == '"' {
			quote = char
		} else if char == '\ufeff' || char != '\u0085' && unicode.IsSpace(char) {
			if value.Len() > 0 {
				out = append(out, value.String())
				value.Reset()
			}
		} else if char == '#' && value.Len() == 0 {
			break
		} else {
			value.WriteRune(char)
		}
	}
	if quote != 0 {
		return nil, errors.New("带引号的文件名未结束")
	}
	if value.Len() > 0 {
		out = append(out, value.String())
	}
	return out, nil
}

var dependencyNumber = regexp.MustCompile(`^[-+]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][-+]?\d+)?$`)
var dependencyNumericOptions = map[string][2]int{"-o": {1, 3}, "-s": {1, 3}, "-t": {1, 3}, "-bm": {1, 1}, "-boost": {1, 1}, "-texres": {1, 1}, "-mm": {2, 2}}

func dependencyTexture(args []string) (string, error) {
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		option := args[i]
		i++
		if numeric, ok := dependencyNumericOptions[option]; ok {
			count := 0
			for count < numeric[1] && i < len(args) && dependencyNumber.MatchString(args[i]) {
				count++
				i++
			}
			if count < numeric[0] {
				return "", fmt.Errorf("MTL %s 参数不完整", option)
			}
		} else if slices.Contains([]string{"-blendu", "-blendv", "-cc", "-clamp", "-imfchan", "-type"}, option) {
			if i >= len(args) || strings.HasPrefix(args[i], "-") {
				return "", fmt.Errorf("MTL %s 参数不完整", option)
			}
			i++
		} else {
			return "", fmt.Errorf("不支持的 MTL 贴图选项 %s，未猜测文件名", option)
		}
	}
	if len(args)-i != 1 {
		return "", errors.New("贴图语句需要一个明确文件名；含空格请加引号")
	}
	return args[i], nil
}
func dependencyReference(from, value string) (string, error) {
	if err := worksetPath(value); err != nil {
		return "", err
	}
	parent := ""
	if i := strings.LastIndex(from, "/"); i >= 0 {
		parent = from[:i+1]
	}
	if err := worksetPath(parent + value); err != nil {
		return "", err
	}
	return parent + value, nil
}
func dependencySort(values []string) {
	slices.SortFunc(values, func(a, b string) int { return slices.Compare(utf16.Encode([]rune(a)), utf16.Encode([]rune(b))) })
}
func meshDependencyText(ctx context.Context, path, text, format string, out *DependencyFileReport) error {
	lines := strings.Split(text, "\n")
	if len(lines) > 10000 {
		return errors.New("文本超过 10,000 行")
	}
	refs := map[string]bool{}
	for i, line := range lines {
		if err := ctx.Err(); err != nil {
			return err
		}
		words, err := dependencyWords(line)
		if err == nil && len(words) > 0 {
			command, args := words[0], words[1:]
			var names []string
			switch {
			case format == "obj" && command == "mtllib":
				if len(args) == 0 {
					err = errors.New("mtllib 缺少文件名")
				} else {
					names = args
				}
			case format == "mtl" && slices.Contains([]string{"map_Ka", "map_Kd", "map_Ks", "map_Ke", "map_Ns", "map_d", "map_Bump", "bump", "norm", "disp", "decal", "refl"}, command):
				var name string
				name, err = dependencyTexture(args)
				if err == nil {
					names = []string{name}
				}
			default:
				known := []string{"newmtl", "Ka", "Kd", "Ks", "Ke", "Ns", "Ni", "illum", "d", "Tr", "Tf"}
				if format == "obj" {
					known = []string{"v", "vt", "vn", "vp", "f", "l", "p", "o", "g", "s", "usemtl"}
				}
				if !slices.Contains(known, command) {
					out.Notices = append(out.Notices, fmt.Sprintf("第 %d 行语句 %s 未识别；未执行或推断其依赖。", i+1, command))
				}
			}
			for _, name := range names {
				var ref string
				ref, err = dependencyReference(path, name)
				if err != nil {
					break
				}
				refs[ref] = true
			}
		}
		if err != nil {
			out.Notices = append(out.Notices, fmt.Sprintf("第 %d 行：%s", i+1, err))
		}
		if len(refs) > 10000 || len(out.Notices) > 1000 {
			return errors.New("依赖或诊断数量超限")
		}
	}
	for ref := range refs {
		out.References = append(out.References, ref)
	}
	dependencySort(out.References)
	return nil
}

var dependencyNodeKey = regexp.MustCompile(`^\d+$`)
var dependencyImageAnnotation = regexp.MustCompile(`\s\[(?:input|output|temp)\]$`)

func dependencyJSONDepth(value any, depth int) bool {
	if depth > 64 {
		return false
	}
	switch v := value.(type) {
	case map[string]any:
		for _, child := range v {
			if !dependencyJSONDepth(child, depth+1) {
				return false
			}
		}
	case []any:
		for _, child := range v {
			if !dependencyJSONDepth(child, depth+1) {
				return false
			}
		}
	}
	return true
}
func comfyDependencyText(ctx context.Context, text, inputRoot string, out *DependencyFileReport) error {
	value, err := canonjson.Decode([]byte(text))
	if err != nil {
		return errors.New("ComfyUI JSON 无效或包含重复字段。")
	}
	if !dependencyJSONDepth(value, 0) {
		return errors.New("JSON 嵌套超过 64 层。")
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return errors.New("仅支持 ComfyUI API 图或 nodes 数组 UI 图")
	}
	nodes, ui := obj["nodes"].([]any)
	if !ui {
		keys := make([]string, 0, len(obj))
		for key := range obj {
			if !dependencyNodeKey.MatchString(key) {
				return errors.New("API 图含非节点顶层字段，未假定其格式")
			}
			keys = append(keys, key)
		}
		dependencySort(keys)
		for _, key := range keys {
			nodes = append(nodes, obj[key])
		}
	}
	if len(nodes) < 1 || len(nodes) > 4096 {
		return errors.New("图需包含 1–4096 个明确节点")
	}
	refs := map[string]bool{}
	out.Format = "comfy-api"
	if ui {
		out.Format = "comfy-ui"
	}
	for i, value := range nodes {
		if err := ctx.Err(); err != nil {
			return err
		}
		node, ok := value.(map[string]any)
		if !ok {
			out.Notices = append(out.Notices, fmt.Sprintf("节点 %d 不是对象。", i+1))
			continue
		}
		typeName := node["class_type"]
		if ui {
			typeName = node["type"]
		}
		if typeName == "LoadImage" {
			var image any
			if ui {
				if widgets, ok := node["widgets_values"].([]any); ok && len(widgets) > 0 {
					image = widgets[0]
				}
			} else if inputs, ok := node["inputs"].(map[string]any); ok {
				image = inputs["image"]
			}
			name, ok := image.(string)
			if !ok {
				out.Notices = append(out.Notices, fmt.Sprintf("LoadImage 节点 %d 缺少明确图片路径。", i+1))
				continue
			}
			switch {
			case inputRoot == "":
				err = errors.New("需显式映射 ComfyUI input 根；不将 JSON 所在目录当作输入根")
			case dependencyImageAnnotation.MatchString(name):
				err = errors.New("目录注记未自动解释，请显式映射实际输入路径")
			default:
				err = worksetPath(name)
				if err == nil {
					if inputRoot != "." {
						name = inputRoot + "/" + name
					}
					err = worksetPath(name)
					if err == nil {
						refs[name] = true
					}
				}
			}
			if err != nil {
				out.Notices = append(out.Notices, fmt.Sprintf("LoadImage 节点 %d：%s", i+1, err))
				err = nil
			}
		} else {
			name, ok := typeName.(string)
			if !ok || !slices.Contains([]string{"ImageScale", "ImageScaleBy", "SaveImage", "PreviewImage", "EmptyImage", "ImageBatch", "InvertImage", "ImagePadForOutpaint", "ImageCrop"}, name) {
				out.Notices = append(out.Notices, fmt.Sprintf("节点 %v 不在已支持范围；模型、自定义节点和其他文件输入未解析。", typeName))
			}
		}
	}
	for ref := range refs {
		out.References = append(out.References, ref)
	}
	dependencySort(out.References)
	return nil
}
