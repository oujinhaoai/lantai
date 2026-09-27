// Package manifest 定义不可变版本清单（lantai.manifest/v1）与资产类型登记，
// 负责把客户端提交的清单规范化、校验并冻结（T02.1）。
//
// 冻结后的内容（Content）按 RFC 8785 规范化计算 manifest_digest，不含台账分配
// 的版本 ID 与版本号，因此可以在台账保留版本之前算出并交给 Prepare；写入版本
// 目录的清单文件（Document）另带身份字段，便于库丢失时由文件重建。本包只做
// 纯判定，不访问存储或台账。
package manifest

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/oujinhaoai/lantai/internal/catalog/pathrule"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/install"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/schemas"
)

// 契约标识。
const (
	Contract      = "lantai.manifest/v1"
	TypesContract = "lantai.asset-types/v1"
)

// AssetType 是资产类型。
type AssetType string

// 资产类型，与 schemas/catalog/v1/manifest.schema.json 一致。
const (
	TypeImage      AssetType = "image"
	TypeVideo      AssetType = "video"
	TypeAudio      AssetType = "audio"
	TypeDoc        AssetType = "doc"
	TypeConfig     AssetType = "config"
	TypePlugin     AssetType = "plugin"
	TypeModel      AssetType = "model"
	TypeMotion     AssetType = "motion"
	TypeScene      AssetType = "scene"
	TypeProduction AssetType = "production"
)

// Types 是全部资产类型。
var Types = []AssetType{TypeImage, TypeVideo, TypeAudio, TypeDoc, TypeConfig, TypePlugin, TypeModel, TypeMotion, TypeScene, TypeProduction}

// Valid 报告 t 是否为登记的类型。
func (t AssetType) Valid() bool { return slices.Contains(Types, t) }

// Roles 是文件角色。
var Roles = []string{"primary", "source", "interchange", "texture", "recipe", "record", "preview", "doc"}

// Relations 是 uses 关系。
var Relations = []string{"uses", "derived_from", "reference"}

// 许可声明的取值。
var (
	Usages        = []string{"production", "reference", "restricted"}
	Sensitivities = []string{"normal", "personal"}
)

// File 是清单中的一个文件。
type File struct {
	Path   string `json:"path"`
	Role   string `json:"role"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Use 是冻结时解析为固定永久引用的输入。
type Use struct {
	InstanceID ids.ID `json:"instance_id"`
	AssetID    ids.ID `json:"asset_id"`
	VersionID  ids.ID `json:"version_id"`
	Relation   string `json:"relation"`
	Declared   string `json:"declared,omitempty"`
}

// Rights 是提交时声明的许可快照。
type Rights struct {
	Usage             string `json:"usage"`
	License           string `json:"license"`
	RedistributeRaw   bool   `json:"redistribute_raw"`
	NoAI              bool   `json:"noai"`
	DigitalSourceType string `json:"digital_source_type,omitempty"`
	Sensitivity       string `json:"sensitivity"`
}

// Content 是冻结的版本内容；manifest_digest 对它计算。
type Content struct {
	Producer      *storage.Producer `json:"producer,omitempty"`
	AssetType     AssetType         `json:"asset_type"`
	TypeSchema    string            `json:"type_schema"`
	BaseVersionID ids.ID            `json:"base_version_id,omitempty"`
	VersionNote   string            `json:"version_note,omitempty"`
	Files         []File            `json:"files"`
	Uses          []Use             `json:"uses"`
	Rights        Rights            `json:"rights"`
	Metadata      map[string]any    `json:"metadata"`
}

// Digest 返回 content 的规范化 SHA-256（manifest_digest）。
func (c Content) Digest() (digest.Digest, error) {
	if err := canonjson.CheckUTF8(c); err != nil {
		return "", err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	canonical, err := canonjson.Canonicalize(raw)
	if err != nil {
		return "", err
	}
	return digest.Of(canonical), nil
}

// InstallFiles 返回交给台账与安装器的文件清单（路径、哈希、大小）。
func (c Content) InstallFiles() []install.File {
	out := make([]install.File, len(c.Files))
	for i, f := range c.Files {
		out[i] = install.File{Path: f.Path, SHA256: f.SHA256, Size: f.Size}
	}
	return out
}

// Document 是版本目录中的清单文件。
type Document struct {
	InstanceID     ids.ID        `json:"instance_id"`
	ProjectID      ids.ID        `json:"project_id"`
	AssetID        ids.ID        `json:"asset_id"`
	VersionID      ids.ID        `json:"version_id"`
	VersionNumber  int64         `json:"version_number"`
	OperationID    ids.ID        `json:"operation_id"`
	CreatedBy      ids.ID        `json:"created_by"`
	SessionID      ids.ID        `json:"session_id,omitempty"`
	ManifestDigest digest.Digest `json:"manifest_digest"`
	Content        Content       `json:"content"`
}

var documentOrder = map[string][]string{
	"": {"contract", "instance_id", "project_id", "asset_id", "version_id", "version_number", "operation_id",
		"created_by", "session_id", "manifest_digest", "content"},
	"/content": {"asset_type", "type_schema", "base_version_id", "version_note", "files", "uses", "rights", "metadata", "producer"},
}

func order(pointer string) []string {
	if o, ok := documentOrder[pointer]; ok {
		return o
	}
	if strings.HasPrefix(pointer, "/content/files/") {
		return []string{"path", "role", "sha256", "size"}
	}
	if strings.HasPrefix(pointer, "/content/uses/") {
		return []string{"instance_id", "asset_id", "version_id", "relation", "declared"}
	}
	return nil
}

// Render 按权威 schema 校验并渲染清单文件（确定性 YAML）。
func (d Document) Render() ([]byte, error) {
	type wire struct {
		Contract string `json:"contract"`
		Document
	}
	got, err := d.Content.Digest()
	if err != nil {
		return nil, err
	}
	if got != d.ManifestDigest {
		return nil, fmt.Errorf("manifest: manifest_digest %s does not match the content (%s)", d.ManifestDigest, got)
	}
	raw, err := Render(wire{Contract: Contract, Document: d}, order)
	if err != nil {
		return nil, err
	}
	if _, err := Decode(raw, Contract); err != nil {
		return nil, fmt.Errorf("manifest: rendered document does not satisfy %s: %w", Contract, err)
	}
	return raw, nil
}

// Parse 解析并校验清单文件，并核对 manifest_digest 与内容一致。
func Parse(raw []byte) (Document, error) {
	tree, err := Decode(raw, Contract)
	if err != nil {
		return Document{}, err
	}
	m := tree.(map[string]any)
	canonical, err := canonicalOf(m["content"])
	if err != nil {
		return Document{}, err
	}
	b, err := json.Marshal(tree)
	if err != nil {
		return Document{}, err
	}
	var d Document
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	if err := dec.Decode(&d); err != nil {
		return Document{}, err
	}
	if digest.Of(canonical) != d.ManifestDigest {
		return Document{}, errcode.New(errcode.HashMismatch, "the manifest content does not match its manifest_digest")
	}
	return d, nil
}

// InputFile 是客户端申报的文件：路径与角色，以及由客户端在本地算出、由
// 上传核验的哈希与大小。
type InputFile struct {
	Path   string `json:"path"`
	Role   string `json:"role"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Input 是待冻结的版本内容；Uses 已由调用方解析为固定引用。
type Input struct {
	Producer      *storage.Producer `json:"producer,omitempty"`
	AssetType     AssetType         `json:"asset_type"`
	BaseVersionID ids.ID            `json:"base_version_id,omitempty"`
	VersionNote   string            `json:"version_note,omitempty"`
	Files         []InputFile       `json:"files"`
	Uses          []Use             `json:"uses,omitempty"`
	Rights        Rights            `json:"rights"`
	Metadata      map[string]any    `json:"metadata,omitempty"`
}

// reservedMetadata 是安全相关、只能在 rights 或专门命令中给出的字段名。
var reservedMetadata = []string{"usage", "license", "sensitivity", "redistribute_raw", "noai",
	"digital_source_type", "rights", "restricted", "restricted_source"}

func pointerErr(code errcode.Code, reason, pointer, message string) *errcode.Error {
	return errcode.New(code, message).WithDetails(errcode.PointerDetail(reason, pointer, message))
}

// Normalize 把输入冻结为规范内容：路径规范化为 NFC 并按字节序排序，拒绝
// 越界、平台冲突与规范化后同名；核对角色、许可声明语法与 uses；元数据中
// 未登记的字段移入 extra（不拒收），安全相关字段一律拒绝；最后按权威 schema
// 校验。
func Normalize(in Input) (Content, error) {
	if err := canonjson.CheckUTF8(in); err != nil {
		return Content{}, pointerErr(errcode.SchemaInvalid, "invalid_utf8", "", "manifest content must be valid UTF-8")
	}
	if !in.AssetType.Valid() {
		return Content{}, pointerErr(errcode.SchemaInvalid, "asset_type", "/asset_type", fmt.Sprintf("unknown asset type %q", in.AssetType))
	}
	if len(in.Files) == 0 {
		return Content{}, pointerErr(errcode.SchemaInvalid, "no_files", "/files", "a version needs at least one file")
	}
	c := Content{Producer: in.Producer, AssetType: in.AssetType, TypeSchema: TypesContract, BaseVersionID: in.BaseVersionID, VersionNote: in.VersionNote}
	if utf8.RuneCountInString(c.VersionNote) > 4000 {
		return Content{}, pointerErr(errcode.SchemaInvalid, "version_note", "/version_note", "version_note is longer than 4000 characters")
	}
	paths := make([]string, 0, len(in.Files))
	for i, f := range in.Files {
		ptr := fmt.Sprintf("/files/%d", i)
		p, err := pathrule.Normalize(f.Path)
		if err != nil {
			if e, ok := errcode.As(err); ok && len(e.Details) > 0 {
				e.Details[0].Pointer = ptrOf(ptr + "/path")
			}
			return Content{}, err
		}
		if !slices.Contains(Roles, f.Role) {
			return Content{}, pointerErr(errcode.SchemaInvalid, "role", ptr+"/role", fmt.Sprintf("unknown file role %q", f.Role))
		}
		if !digest.ValidHex(f.SHA256) {
			return Content{}, pointerErr(errcode.SchemaInvalid, "sha256", ptr+"/sha256", "sha256 must be 64 lowercase hex characters")
		}
		if f.Size < 0 {
			return Content{}, pointerErr(errcode.SchemaInvalid, "size", ptr+"/size", "size must not be negative")
		}
		c.Files = append(c.Files, File{Path: p, Role: f.Role, SHA256: f.SHA256, Size: f.Size})
		paths = append(paths, p)
	}
	if err := pathrule.CheckSet(paths); err != nil {
		return Content{}, err
	}
	sort.Slice(c.Files, func(i, j int) bool { return c.Files[i].Path < c.Files[j].Path })
	uses, err := normalizeUses(in.Uses)
	if err != nil {
		return Content{}, err
	}
	c.Uses = uses
	if err := checkRights(in.Rights); err != nil {
		return Content{}, err
	}
	c.Rights = in.Rights
	md, err := normalizeMetadata(in.AssetType, in.Metadata)
	if err != nil {
		return Content{}, err
	}
	c.Metadata = md
	if err := validateContent(c); err != nil {
		return Content{}, err
	}
	return c, nil
}

func ptrOf(s string) *string { return &s }

func normalizeUses(in []Use) ([]Use, error) {
	out := make([]Use, 0, len(in))
	seen := map[[3]string]bool{}
	for i, u := range in {
		ptr := fmt.Sprintf("/uses/%d", i)
		if !u.InstanceID.Valid() || !u.AssetID.Valid() || !u.VersionID.Valid() {
			return nil, pointerErr(errcode.SchemaInvalid, "use_ref", ptr, "uses must be resolved to permanent references")
		}
		if !slices.Contains(Relations, u.Relation) {
			return nil, pointerErr(errcode.SchemaInvalid, "relation", ptr+"/relation", fmt.Sprintf("unknown relation %q", u.Relation))
		}
		k := [3]string{string(u.AssetID), string(u.VersionID), u.Relation}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.AssetID != b.AssetID {
			return a.AssetID < b.AssetID
		}
		if a.VersionID != b.VersionID {
			return a.VersionID < b.VersionID
		}
		return a.Relation < b.Relation
	})
	return out, nil
}

var digitalSourceRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9]{0,63}$`)

func checkRights(r Rights) error {
	switch {
	case !slices.Contains(Usages, r.Usage):
		return pointerErr(errcode.SchemaInvalid, "usage", "/rights/usage", fmt.Sprintf("usage must be one of %v", Usages))
	case !slices.Contains(Sensitivities, r.Sensitivity):
		return pointerErr(errcode.SchemaInvalid, "sensitivity", "/rights/sensitivity", fmt.Sprintf("sensitivity must be one of %v", Sensitivities))
	case r.DigitalSourceType != "" && !digitalSourceRE.MatchString(r.DigitalSourceType):
		return pointerErr(errcode.SchemaInvalid, "digital_source_type", "/rights/digital_source_type", "digital_source_type must be an IPTC term")
	}
	if err := CheckLicense(r.License); err != nil {
		return pointerErr(errcode.SchemaInvalid, "license", "/rights/license", err.Error())
	}
	return nil
}

var typeProperties = func() map[AssetType][]string {
	raw, err := schemas.FS.ReadFile("catalog/v1/asset-types.schema.json")
	if err != nil {
		panic(err)
	}
	var lib struct {
		Defs map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &lib); err != nil {
		panic(err)
	}
	out := map[AssetType][]string{}
	for _, t := range Types {
		def, ok := lib.Defs[string(t)]
		if !ok {
			panic("manifest: asset type " + string(t) + " has no definition in " + TypesContract)
		}
		for k := range def.Properties {
			out[t] = append(out[t], k)
		}
	}
	return out
}()

// KnownMetadata 返回类型登记的元数据字段名（含 extra）。
func KnownMetadata(t AssetType) []string {
	out := slices.Clone(typeProperties[t])
	sort.Strings(out)
	return out
}

func normalizeMetadata(t AssetType, in map[string]any) (map[string]any, error) {
	out := map[string]any{}
	extra := map[string]any{}
	if e, ok := in["extra"]; ok {
		m, isMap := e.(map[string]any)
		if !isMap {
			return nil, pointerErr(errcode.SchemaInvalid, "extra", "/metadata/extra", "metadata.extra must be an object")
		}
		for k, v := range m {
			if slices.Contains(reservedMetadata, k) {
				return nil, pointerErr(errcode.SchemaInvalid, "reserved_metadata_field", "/metadata/extra/"+escapePointer(k),
					fmt.Sprintf("%q is a rights field; declare it in rights, not in metadata.extra", k))
			}
			extra[k] = v
		}
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := in[k]
		switch {
		case k == "extra":
		case slices.Contains(reservedMetadata, k):
			return nil, pointerErr(errcode.SchemaInvalid, "reserved_metadata_field", "/metadata/"+escapePointer(k),
				fmt.Sprintf("%q is a rights field; declare it in rights, not in metadata", k))
		case slices.Contains(typeProperties[t], k):
			out[k] = v
		default:
			// 未登记的字段不拒收，放进 extra；与 extra 中已有的同名值冲突时拒绝。
			if prev, ok := extra[k]; ok {
				a, _ := json.Marshal(prev)
				b, _ := json.Marshal(v)
				if string(a) != string(b) {
					return nil, pointerErr(errcode.SchemaInvalid, "extra_conflict", "/metadata/"+escapePointer(k),
						fmt.Sprintf("%q also appears in metadata.extra with a different value", k))
				}
			}
			extra[k] = v
		}
	}
	if len(extra) > 0 {
		out["extra"] = extra
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, pointerErr(errcode.SchemaInvalid, "metadata", "/metadata", "metadata must be JSON-serializable")
	}
	tree, err := canonjson.Decode(raw)
	if err != nil {
		return nil, pointerErr(errcode.SchemaInvalid, "metadata", "/metadata", err.Error())
	}
	reg, err := schema.Default()
	if err != nil {
		return nil, err
	}
	if err := reg.Validate(TypesContract+"#/$defs/"+string(t), tree); err != nil {
		return nil, errcode.Wrap(errcode.SchemaInvalid, "metadata does not satisfy the "+string(t)+" type", err)
	}
	// 返回独立的 JSON 数据树，调用方之后修改输入映射不改变已冻结的内容。
	return tree.(map[string]any), nil
}

func validateContent(c Content) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tree, err := canonjson.Decode(raw)
	if err != nil {
		return errcode.Wrap(errcode.SchemaInvalid, "manifest content is not valid JSON", err)
	}
	reg, err := schema.Default()
	if err != nil {
		return err
	}
	if err := reg.Validate(Contract+"#/$defs/content", tree); err != nil {
		return errcode.Wrap(errcode.SchemaInvalid, "manifest content does not satisfy "+Contract, err)
	}
	return nil
}
