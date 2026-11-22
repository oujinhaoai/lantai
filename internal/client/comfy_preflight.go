package client

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/oujinhaoai/lantai/internal/catalog/pathrule"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// ComfyPreflightInput compares a selected graph against a past completed pull.
// Roots are explicit workset-relative directory names, never filesystem paths.
type ComfyPreflightInput struct {
	Workflow       string            `json:"workflow"`
	ComfyInputRoot string            `json:"comfy_input_root,omitempty"`
	ModelRoots     map[string]string `json:"model_roots,omitempty"`
	Workset        WorksetReceipt    `json:"workset"`
}

type ComfyPreflightMatch struct {
	AssetID        string        `json:"asset_id"`
	VersionID      string        `json:"version_id"`
	ManifestDigest digest.Digest `json:"manifest_digest"`
	Destination    string        `json:"destination"`
	SourcePath     string        `json:"source_path"`
	SHA256         string        `json:"sha256"`
	Size           int64         `json:"size"`
}

type ComfyPreflightFinding struct {
	Node     int                  `json:"node"` // one-based in parser order, not a graph node ID
	NodeType string               `json:"node_type,omitempty"`
	Input    string               `json:"input,omitempty"`
	Kind     string               `json:"kind,omitempty"`
	Path     string               `json:"path,omitempty"`
	Status   string               `json:"status"`
	Reason   string               `json:"reason,omitempty"`
	Match    *ComfyPreflightMatch `json:"match,omitempty"`
}

type ComfyPreflightReport struct {
	Scope              string                  `json:"scope"`
	Workflow           string                  `json:"workflow"`
	WorkflowSHA256     string                  `json:"workflow_sha256"`
	WorkflowInReceipt  bool                    `json:"workflow_in_receipt"`
	Format             string                  `json:"format"`
	WorksetInputDigest digest.Digest           `json:"workset_input_digest"`
	WorksetInstanceID  string                  `json:"workset_instance_id"`
	Findings           []ComfyPreflightFinding `json:"findings"`
	DraftEntries       []WorksetEntry          `json:"draft_entries"`
	DraftDependencies  []WorksetDependency     `json:"draft_dependencies"`
}

type comfyLoader struct{ field, kind string }

func comfyKnownLoader(name string) (comfyLoader, bool) {
	switch name {
	case "LoadImage":
		return comfyLoader{"image", "images"}, true
	case "CheckpointLoaderSimple":
		return comfyLoader{"ckpt_name", "checkpoints"}, true
	case "LoraLoader":
		return comfyLoader{"lora_name", "loras"}, true
	case "VAELoader":
		return comfyLoader{"vae_name", "vae"}, true
	}
	return comfyLoader{}, false
}

// AnalyzeComfyPreflight reads exactly one selected regular JSON file. All
// dependency checks use receipt metadata: no native runtime, transfer, file
// probes, or authorization claims.
func AnalyzeComfyPreflight(ctx context.Context, in ComfyPreflightInput, directory string) (ComfyPreflightReport, error) {
	var out ComfyPreflightReport
	if err := worksetPath(in.Workflow); err != nil {
		return out, err
	}
	if !strings.EqualFold(filepath.Ext(in.Workflow), ".json") {
		return out, worksetInvalid("select a ComfyUI JSON workflow")
	}
	for k, root := range in.ModelRoots {
		if k != "checkpoints" && k != "loras" && k != "vae" {
			return out, worksetInvalid("unknown ComfyUI model root category")
		}
		if root == "" {
			return out, worksetInvalid("explicit model root cannot be empty")
		}
		if root != "." {
			if err := worksetPath(root); err != nil {
				return out, err
			}
		}
	}
	if in.ComfyInputRoot != "" && in.ComfyInputRoot != "." {
		if err := worksetPath(in.ComfyInputRoot); err != nil {
			return out, err
		}
	}
	index, spellings, instance, err := comfyReceiptInventory(in.Workset)
	if err != nil {
		return out, err
	}
	directory, err = importAbsolute(directory)
	if err != nil {
		return out, err
	}
	root, err := openWorksetRoot(directory, false)
	if err != nil {
		return out, err
	}
	if root == nil {
		return out, worksetInvalid("ComfyUI source directory does not exist")
	}
	defer root.Close()
	files, err := readDependencySources(ctx, root, []string{in.Workflow})
	if err != nil {
		return out, err
	}
	raw := files[0].Bytes
	if !utf8.Valid(raw) || bytes.IndexByte(raw, 0) >= 0 {
		return out, worksetInvalid("ComfyUI workflow requires UTF-8 JSON without NUL")
	}
	nodes, ui, err := decodeComfyNodes(string(bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})))
	if err != nil {
		return out, err
	}
	sha := importSHA(raw)
	wf, inReceipt := index[in.Workflow]
	if inReceipt && (wf.SHA256 != sha || wf.Size != int64(len(raw))) {
		return out, worksetInvalid("selected workflow differs from its fixed receipt file")
	}
	out = ComfyPreflightReport{
		Scope: "fixed-workset-receipt-only", Workflow: in.Workflow, WorkflowSHA256: sha,
		WorkflowInReceipt: inReceipt, Format: "comfy-api",
		WorksetInputDigest: in.Workset.InputDigest, WorksetInstanceID: instance,
		Findings: []ComfyPreflightFinding{}, DraftEntries: []WorksetEntry{}, DraftDependencies: []WorksetDependency{},
	}
	if ui {
		out.Format = "comfy-ui"
	}
	seenEntries := map[WorksetEntry]bool{}
	seenPaths := map[string]bool{}
	var requires []string
	for i, node := range nodes {
		if err := ctx.Err(); err != nil {
			return ComfyPreflightReport{}, err
		}
		f, relevant := comfyPreflightNode(i+1, node, ui, in, index, spellings)
		if relevant {
			out.Findings = append(out.Findings, f)
		}
		if f.Match != nil {
			entry := WorksetEntry{AssetID: f.Match.AssetID, VersionID: f.Match.VersionID, Destination: f.Match.Destination}
			if !seenEntries[entry] {
				seenEntries[entry] = true
				out.DraftEntries = append(out.DraftEntries, entry)
			}
			if !seenPaths[f.Path] && f.Path != in.Workflow {
				seenPaths[f.Path] = true
				requires = append(requires, f.Path)
			}
		}
	}
	if len(requires) > 0 {
		slices.Sort(requires)
		out.DraftDependencies = append(out.DraftDependencies, WorksetDependency{From: in.Workflow, Requires: requires})
	}
	return out, nil
}

func comfyPreflightNode(i int, value any, ui bool, in ComfyPreflightInput, index map[string]ComfyPreflightMatch, spellings map[string]string) (ComfyPreflightFinding, bool) {
	f := ComfyPreflightFinding{Node: i, Status: "manual"}
	node, ok := value.(map[string]any)
	if !ok {
		f.Reason = "invalid_node"
		return f, true
	}
	typeField := "class_type"
	if ui {
		typeField = "type"
	}
	name, _ := node[typeField].(string)
	// Do not echo arbitrary long or control-containing customized type names.
	if len(name) <= 128 && !strings.ContainsAny(name, "\r\n\x00") {
		f.NodeType = name
	}
	loader, known := comfyKnownLoader(name)
	if !known {
		if comfySafeImageNode(name) {
			return f, false
		}
		f.Reason = "unsupported_node"
		return f, true
	}
	f.Input, f.Kind = loader.field, loader.kind
	if ui && loader.kind != "images" {
		f.Reason = "ui_model_widget_unresolved"
		return f, true
	}
	var valueInput any
	if ui {
		if inputs, ok := node["inputs"].([]any); ok {
			for _, value := range inputs {
				slot, _ := value.(map[string]any)
				if n, _ := slot["name"].(string); n == loader.field && slot["link"] != nil {
					f.Reason = "not_literal"
					return f, true
				}
			}
		}
		if widgets, ok := node["widgets_values"].([]any); ok && len(widgets) > 0 {
			valueInput = widgets[0]
		}
	} else if inputs, ok := node["inputs"].(map[string]any); ok {
		valueInput = inputs[loader.field]
	}
	name, ok = valueInput.(string)
	if !ok {
		f.Reason = "not_literal"
		return f, true
	}
	root := in.ModelRoots[loader.kind]
	if loader.kind == "images" {
		root = in.ComfyInputRoot
	}
	if root == "" {
		f.Reason = "root_not_mapped"
		return f, true
	}
	if dependencyImageAnnotation.MatchString(name) || worksetPath(name) != nil {
		f.Reason = "unsafe_or_annotated_path"
		return f, true
	}
	if loader.kind != "images" {
		switch strings.ToLower(filepath.Ext(name)) {
		case ".safetensors", ".ckpt", ".pt", ".pth", ".bin":
		default:
			f.Reason = "model_filename_unresolved"
			return f, true
		}
	}
	if root != "." {
		name = root + "/" + name
	}
	if worksetPath(name) != nil {
		f.Reason = "unsafe_or_annotated_path"
		return f, true
	}
	f.Path = name
	if m, ok := index[name]; ok {
		f.Status, f.Match = "present", &m
	} else if comfySpellingConflict(name, index, spellings) {
		f.Reason = "path_spelling_or_prefix_conflict"
	} else {
		f.Status = "missing"
	}
	return f, true
}

func comfySpellingConflict(name string, index map[string]ComfyPreflightMatch, spellings map[string]string) bool {
	parts := strings.Split(name, "/")
	for i := range parts {
		p := strings.Join(parts[:i+1], "/")
		wire, ok := spellings[pathrule.Key(p)]
		_, occupiedFile := index[p]
		if ok && (p != wire || i == len(parts)-1 || occupiedFile) {
			return true
		}
	}
	return false
}

func comfyReceiptInventory(r WorksetReceipt) (map[string]ComfyPreflightMatch, map[string]string, string, error) {
	var def WorksetInput
	def.Schema, def.Entrypoints, def.Dependencies = WorksetSchema, r.Entrypoints, r.Dependencies
	for _, e := range r.Entries {
		def.Entries = append(def.Entries, e.WorksetEntry)
	}
	if r.Schema != WorksetReceiptSchema || r.State != "completed" || !r.InputDigest.Valid() {
		return nil, nil, "", worksetInvalid("a completed fixed Workset receipt is required")
	}
	if err := validateWorksetDefinition(def); err != nil {
		return nil, nil, "", err
	}
	raw, err := json.Marshal(def)
	if err != nil {
		return nil, nil, "", err
	}
	canonical, err := canonjson.Canonicalize(raw)
	if err != nil || digest.Of(canonical) != r.InputDigest {
		return nil, nil, "", worksetInvalid("Workset receipt input digest does not match its fixed input")
	}
	instance := r.Entries[0].InstanceID
	if !ids.ID(instance).Valid() || r.FileCount < 1 || r.FileCount > MaxWorksetFiles || r.TotalBytes < 0 || r.TotalBytes > MaxWorksetBytes {
		return nil, nil, "", worksetInvalid("invalid Workset receipt identity or totals")
	}
	index := map[string]ComfyPreflightMatch{}
	spellings := map[string]string{}
	paths := []string{}
	var total int64
	for _, e := range r.Entries {
		if e.InstanceID != instance || !e.ManifestDigest.Valid() || len(e.Files) == 0 {
			return nil, nil, "", worksetInvalid("incomplete or mixed-instance Workset receipt")
		}
		for _, f := range e.Files {
			if f.State != "verified" || !digest.ValidHex(f.SHA256) || f.Size < 0 || f.Size > MaxWorksetBytes-total ||
				f.Path != e.Destination+"/"+f.SourcePath || worksetPath(f.SourcePath) != nil || worksetPath(f.Path) != nil {
				return nil, nil, "", worksetInvalid("invalid or unverified Workset receipt file")
			}
			total += f.Size
			paths = append(paths, f.Path)
			if len(paths) > MaxWorksetFiles {
				return nil, nil, "", worksetInvalid("Workset receipt file count exceeds " + strconv.Itoa(MaxWorksetFiles))
			}
			index[f.Path] = ComfyPreflightMatch{e.AssetID, e.VersionID, e.ManifestDigest, e.Destination, f.SourcePath, f.SHA256, f.Size}
			parts := strings.Split(f.Path, "/")
			for i := range parts {
				p := strings.Join(parts[:i+1], "/")
				spellings[pathrule.Key(p)] = p
			}
		}
	}
	if len(paths) != r.FileCount || total != r.TotalBytes {
		return nil, nil, "", worksetInvalid("Workset receipt totals do not match its files")
	}
	if worksetConflicts(paths) {
		return nil, nil, "", worksetConflict("Workset receipt paths conflict")
	}
	if err := checkWorksetDependencies(def, paths); err != nil {
		return nil, nil, "", err
	}
	return index, spellings, instance, nil
}

func comfySafeImageNode(name string) bool {
	return slices.Contains([]string{"ImageScale", "ImageScaleBy", "SaveImage", "PreviewImage", "EmptyImage", "ImageBatch", "InvertImage", "ImagePadForOutpaint", "ImageCrop"}, name)
}
