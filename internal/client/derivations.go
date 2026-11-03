package client

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"github.com/oujinhaoai/lantai/internal/assettools"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

const DerivationInputSchema = "lantai.asset-derivation-input/v1"
const DerivationReceiptSchema = "lantai.asset-derivation-receipt/v1"
const DerivationTool = "lantai.bounded-asset-tools/v1"
const DerivationFiles = 256
const DerivationBytes = 64 << 20

type FixedVersionRef struct {
	AssetID   string `json:"asset_id"`
	VersionID string `json:"version_id"`
}
type DerivationSource struct {
	FixedVersionRef
	Path string `json:"path"`
}
type DerivationInput struct {
	Schema  string            `json:"schema"`
	Action  string            `json:"action"`
	Purpose string            `json:"purpose"`
	Source  *DerivationSource `json:"source,omitempty"`
	Before  *FixedVersionRef  `json:"before,omitempty"`
	After   *FixedVersionRef  `json:"after,omitempty"`
	Workset *WorksetInput     `json:"workset,omitempty"`
	Edge    int               `json:"edge,omitempty"`
}
type DerivationProof struct {
	FixedVersionRef
	InstanceID     string `json:"instance_id"`
	ManifestDigest string `json:"manifest_digest"`
	FilesDigest    string `json:"files_digest"`
	Files          int    `json:"files"`
	Bytes          int64  `json:"bytes"`
}
type DerivationArtifact struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Format string `json:"format"`
}
type DerivationReceipt struct {
	Schema                             string             `json:"schema"`
	Tool                               string             `json:"tool"`
	Action                             string             `json:"action"`
	Purpose                            string             `json:"purpose"`
	Parameters                         map[string]any     `json:"parameters"`
	Inputs                             []DerivationProof  `json:"inputs"`
	Artifact                           DerivationArtifact `json:"artifact"`
	CurrentRemoteVerified              bool               `json:"current_remote_verified"`
	CompletionBasis                    string             `json:"completion_basis"`
	NativeSoftwareContinuationVerified bool               `json:"native_software_continuation_verified"`
	SourceRetirementAuthorized         bool               `json:"source_retirement_authorized"`
}
type FileDifference struct {
	Path   string     `json:"path"`
	Kind   string     `json:"kind"`
	Before *InputFile `json:"before,omitempty"`
	After  *InputFile `json:"after,omitempty"`
}
type FieldDifference struct {
	Field   string          `json:"field"`
	Changed bool            `json:"changed"`
	Before  json.RawMessage `json:"before"`
	After   json.RawMessage `json:"after"`
}
type VersionComparison struct {
	Schema string            `json:"schema"`
	Files  []FileDifference  `json:"files"`
	Fields []FieldDifference `json:"fields"`
}
type derivationManifest struct {
	ref     FixedVersionRef
	plan    WorksetPlannedEntry
	content map[string]json.RawMessage
	files   []InputFile
}

func (c *Client) derivationManifest(ctx context.Context, ref FixedVersionRef, instance string) (derivationManifest, error) {
	out := derivationManifest{ref: ref}
	if !ids.ID(ref.AssetID).Valid() || !ids.ID(ref.VersionID).Valid() {
		return out, worksetInvalid("exact derivation version required")
	}
	plan, err := c.worksetManifest(ctx, WorksetEntry{AssetID: ref.AssetID, VersionID: ref.VersionID, Destination: "selected"}, instance)
	if err != nil {
		return out, err
	}
	out.plan = plan
	r, err := c.Do(ctx, http.MethodGet, "/api/v1/assets/"+ref.AssetID+"/versions/"+ref.VersionID+"?view=full", nil, Options{})
	if err != nil {
		return out, err
	}
	var body struct {
		Manifest struct {
			Content json.RawMessage `json:"content"`
		} `json:"manifest"`
	}
	if err := r.Decode(&body); err != nil {
		return out, err
	}
	raw, err := canonjson.Canonicalize(body.Manifest.Content)
	if err != nil {
		return out, err
	}
	if digest.Of(raw) != plan.ManifestDigest {
		return out, errcode.New(errcode.RefMismatch, "derivation manifest changed")
	}
	if err := json.Unmarshal(body.Manifest.Content, &out.content); err != nil {
		return out, err
	}
	if err := json.Unmarshal(out.content["files"], &out.files); err != nil {
		return out, err
	}
	return out, nil
}
func derivationProof(m derivationManifest, files []InputFile) DerivationProof {
	sorted := append([]InputFile(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	raw, _ := canonjson.CanonicalizeValue(sorted)
	var size int64
	for _, file := range files {
		size += file.Size
	}
	return DerivationProof{FixedVersionRef: m.ref, InstanceID: m.plan.InstanceID, ManifestDigest: string(m.plan.ManifestDigest), FilesDigest: string(digest.Of(raw)), Files: len(files), Bytes: size}
}
func compareDerivationManifests(before, after derivationManifest) VersionComparison {
	out := VersionComparison{Schema: "lantai.version-file-comparison/v1", Files: []FileDifference{}, Fields: []FieldDifference{}}
	left, right := map[string]InputFile{}, map[string]InputFile{}
	paths := map[string]bool{}
	for _, f := range before.files {
		left[f.Path] = f
		paths[f.Path] = true
	}
	for _, f := range after.files {
		right[f.Path] = f
		paths[f.Path] = true
	}
	names := make([]string, 0, len(paths))
	for path := range paths {
		names = append(names, path)
	}
	sort.Strings(names)
	for _, path := range names {
		a, okA := left[path]
		b, okB := right[path]
		row := FileDifference{Path: path, Kind: "unchanged"}
		if okA {
			row.Before = &a
		}
		if okB {
			row.After = &b
		}
		switch {
		case !okA:
			row.Kind = "added"
		case !okB:
			row.Kind = "removed"
		case a != b:
			row.Kind = "changed"
		}
		out.Files = append(out.Files, row)
	}
	for _, field := range []string{"asset_type", "version_note", "uses", "rights", "metadata", "producer"} {
		a, b := before.content[field], after.content[field]
		if len(a) == 0 {
			a = json.RawMessage("null")
		}
		if len(b) == 0 {
			b = json.RawMessage("null")
		}
		ca, _ := canonjson.Canonicalize(a)
		cb, _ := canonjson.Canonicalize(b)
		out.Fields = append(out.Fields, FieldDifference{Field: field, Changed: !bytes.Equal(ca, cb), Before: a, After: b})
	}
	return out
}

// DeriveTo publishes only to an already pinned, caller-selected private output.
// Every invocation rechecks remote access and intended purpose before any cached
// input can produce a new artifact. Outputs never update an asset or approval.
func (c *Client) DeriveTo(ctx context.Context, in DerivationInput, directory string, output *PinnedPrivateOutput) (DerivationReceipt, error) {
	out := DerivationReceipt{Schema: DerivationReceiptSchema, Tool: DerivationTool, Action: in.Action, Purpose: in.Purpose, Parameters: map[string]any{}, Inputs: []DerivationProof{}, CompletionBasis: "current_remote_and_downloaded_files"}
	if in.Schema != DerivationInputSchema {
		return out, worksetInvalid("unsupported derivation input schema")
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	rootPath, err := importAbsolute(directory)
	if err != nil {
		return out, err
	}
	root, err := openWorksetRoot(rootPath, false)
	if err != nil {
		return out, err
	}
	if root == nil {
		return out, worksetInvalid("explicit existing derivation directory required")
	}
	defer root.Close()
	if output == nil || output.root == nil {
		return out, worksetInvalid("pinned artifact output required")
	}
	if err := importDisjoint(rootPath, filepath.Join(output.parent, output.name)); err != nil {
		return out, err
	}
	instance, err := c.worksetInstance(ctx)
	if err != nil {
		return out, err
	}
	var recheck []derivationManifest
	var artifact []byte
	format := "application/json"
	if in.Action == "compare" || in.Action == "workset_tar" {
		if in.Source != nil || in.Edge != 0 {
			return out, worksetInvalid("whole-version operations cannot include image source/edge")
		}
		var input WorksetInput
		var manifests []derivationManifest
		if in.Action == "compare" {
			if in.Purpose != "archive_review" || in.Before == nil || in.After == nil || in.Workset != nil {
				return out, worksetInvalid("compare requires two exact versions and archive_review")
			}
			input = WorksetInput{Schema: WorksetSchema, Entries: []WorksetEntry{{AssetID: in.Before.AssetID, VersionID: in.Before.VersionID, Destination: "before"}, {AssetID: in.After.AssetID, VersionID: in.After.VersionID, Destination: "after"}}}
		} else {
			if in.Purpose != "raw_export" || in.Workset == nil || in.Before != nil || in.After != nil {
				return out, worksetInvalid("workset_tar requires an explicit workset and raw_export")
			}
			input = *in.Workset
		}
		plan, err := c.planWorkset(ctx, input, rootPath, DerivationFiles, DerivationBytes)
		if err != nil {
			return out, err
		}
		if plan.FileCount > DerivationFiles || plan.TotalBytes > DerivationBytes {
			return out, worksetInvalid("derivation limit is256 files and64MiB selected bytes")
		}
		for _, entry := range input.Entries {
			m, err := c.derivationManifest(ctx, FixedVersionRef{AssetID: entry.AssetID, VersionID: entry.VersionID}, instance)
			if err != nil {
				return out, err
			}
			manifests = append(manifests, m)
			out.Inputs = append(out.Inputs, derivationProof(m, m.files))
			recheck = append(recheck, m)
		}
		// PullWorkset deliberately reacquires every grant and checks existing bytes.
		pulled, err := c.pullWorkset(ctx, input, rootPath, in.Purpose, DerivationFiles, DerivationBytes)
		if err != nil {
			return out, err
		}
		if len(pulled.Entries) != len(manifests) {
			return out, errcode.New(errcode.RefMismatch, "derivation selection changed during pull")
		}
		for i, entry := range pulled.Entries {
			if entry.ManifestDigest != manifests[i].plan.ManifestDigest || entry.InstanceID != instance {
				return out, errcode.New(errcode.RefMismatch, "derivation manifest changed during pull")
			}
		}
		if pulled.State != "completed" {
			return out, errors.New("derivation selected files not complete")
		}
		if in.Action == "compare" {
			artifact, err = json.MarshalIndent(compareDerivationManifests(manifests[0], manifests[1]), "", "  ")
			if err != nil {
				return out, err
			}
		} else {
			out.Parameters["format"] = "pax"
			out.Parameters["mode"] = "0600"
			out.Parameters["mtime"] = 0
			// Archive only exact planned paths, never enumerate arbitrary local files.
			var buffer bytes.Buffer
			writer := tar.NewWriter(&buffer)
			for _, entry := range pulled.Entries {
				for _, file := range entry.Files {
					if err := ctx.Err(); err != nil {
						return out, err
					}
					header := &tar.Header{Name: file.Path, Mode: 0600, Size: file.Size, Format: tar.FormatPAX, Typeflag: tar.TypeReg}
					if err := writer.WriteHeader(header); err != nil {
						return out, err
					}
					f, err := root.Open(filepath.FromSlash(file.Path))
					if err != nil {
						return out, err
					}
					hash, size, err := hashFile(ctx, f)
					if err == nil && (hash != file.SHA256 || size != file.Size) {
						err = errcode.New(errcode.HashMismatch, "working copy changed before archive")
					}
					if err == nil {
						_, err = f.Seek(0, io.SeekStart)
					}
					if err == nil {
						var copied int64
						actual := digest.NewHasher()
						copied, err = io.Copy(io.MultiWriter(writer, actual), contextReader{ctx, f})
						if err == nil && (actual.Hex() != file.SHA256 || actual.Size() != file.Size) {
							err = errcode.New(errcode.HashMismatch, "bytes changed while archiving")
						}
						if err == nil && copied != file.Size {
							err = io.ErrUnexpectedEOF
						}
					}
					_ = f.Close()
					if err != nil {
						return out, err
					}
				}
			}
			if err := writer.Close(); err != nil {
				return out, err
			}
			artifact = buffer.Bytes()
			format = "application/x-tar"
		}
	} else {
		if in.Purpose != "archive_review" || in.Source == nil || in.Before != nil || in.After != nil || in.Workset != nil {
			return out, worksetInvalid("preview requires an exact source path and archive_review")
		}
		edge := in.Edge
		if edge == 0 {
			edge = 1024
			if in.Action == "thumbnail" {
				edge = 320
			}
		}
		if edge != 320 && edge != 512 && edge != 1024 && edge != 2048 {
			return out, worksetInvalid("edge must be320,512,1024or2048")
		}
		if in.Action != "thumbnail" && in.Action != "proxy" && in.Action != "obj_wireframe" && in.Action != "obj_structure" {
			return out, worksetInvalid("unsupported bounded derivation action")
		}
		if err := worksetPath(in.Source.Path); err != nil {
			return out, err
		}
		m, err := c.derivationManifest(ctx, in.Source.FixedVersionRef, instance)
		if err != nil {
			return out, err
		}
		var selected *InputFile
		for i := range m.files {
			if m.files[i].Path == in.Source.Path {
				selected = &m.files[i]
				break
			}
		}
		if selected == nil {
			return out, errcode.New(errcode.NotFound, "exact selected file absent from manifest")
		}
		limit := int64(assettools.ImageBytes)
		if strings.HasPrefix(in.Action, "obj_") {
			limit = assettools.OBJBytes
		}
		if selected.Size > limit {
			return out, worksetInvalid("selected source exceeds derivation byte budget")
		}
		if err := checkWorksetTarget(root, selected.Path, false); err != nil {
			return out, err
		}
		linkResponse, err := c.FileDownloadLink(ctx, m.ref.AssetID, m.ref.VersionID, selected.Path, in.Purpose)
		if err != nil {
			return out, err
		}
		var link DownloadLink
		if err := linkResponse.Decode(&link); err != nil {
			return out, err
		}
		if link.SHA256 != selected.SHA256 || link.Size != selected.Size {
			return out, errcode.New(errcode.RefMismatch, "derivation read grant differs from exact manifest")
		}
		if err := checkWorksetRoot(rootPath, root); err != nil {
			return out, err
		}
		if err := c.downloadRoot(ctx, DownloadGrant{URL: link.URL, SHA256: link.SHA256, Size: link.Size, Path: link.Path}, root, selected.Path); err != nil {
			return out, err
		}
		f, err := root.Open(filepath.FromSlash(selected.Path))
		if err != nil {
			return out, err
		}
		raw, err := io.ReadAll(io.LimitReader(contextReader{ctx, f}, limit+1))
		_ = f.Close()
		if err != nil {
			return out, err
		}
		if int64(len(raw)) != selected.Size || importSHA(raw) != selected.SHA256 {
			return out, errcode.New(errcode.HashMismatch, "selected local bytes changed")
		}
		out.Inputs = append(out.Inputs, derivationProof(m, []InputFile{*selected}))
		m.files = []InputFile{*selected}
		recheck = append(recheck, m)
		out.Parameters["source_path"] = selected.Path
		out.Parameters["edge"] = edge
		if in.Action == "thumbnail" || in.Action == "proxy" {
			var info assettools.ImageInfo
			artifact, info, err = assettools.ImageProxy(ctx, raw, edge)
			out.Parameters["image"] = info
			out.Parameters["sampling"] = "nearest_neighbor"
			out.Parameters["orientation"] = "decoded_pixels"
			format = "image/png"
		} else {
			var wireframe assettools.Wireframe
			wireframe, err = assettools.ParseOBJ(ctx, raw)
			if err == nil {
				out.Parameters["vertices"] = len(wireframe.Vertices)
				out.Parameters["edges"] = len(wireframe.Edges)
				out.Parameters["faces"] = wireframe.Faces
				if in.Action == "obj_structure" {
					artifact, err = wireframe.JSON()
				} else {
					artifact, err = wireframe.SVG(ctx, edge)
					format = "image/svg+xml"
					out.Parameters["projection"] = "fixed_isometric"
				}
			}
		}
		if err != nil {
			return out, err
		}
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if err := checkWorksetRoot(rootPath, root); err != nil {
		return out, err
	}
	if len(artifact) > assettools.MaxArtifactBytes {
		return out, worksetInvalid("artifact byte budget exceeded")
	}
	// Reacquire current purpose/authority after transformation as well as before
	// reading cached bytes. A rejected source never silently changes purpose.
	for _, m := range recheck {
		for _, file := range m.files {
			r, err := c.FileDownloadLink(ctx, m.ref.AssetID, m.ref.VersionID, file.Path, in.Purpose)
			if err != nil {
				return out, err
			}
			var link DownloadLink
			if err := r.Decode(&link); err != nil {
				return out, err
			}
			if link.SHA256 != file.SHA256 || link.Size != file.Size {
				return out, errcode.New(errcode.RefMismatch, "source grant changed before artifact publication")
			}
		}
	}
	if err := checkWorksetRoot(rootPath, root); err != nil {
		return out, err
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if err := output.WriteBinaryOriginal(artifact); err != nil {
		return out, err
	}
	// The private publisher verified the inode and original byte equality. A
	// receipt describes this invocation only, never continuing remote authority.
	out.Artifact = DerivationArtifact{SHA256: importSHA(artifact), Size: int64(len(artifact)), Format: format}
	out.CurrentRemoteVerified = true
	return out, nil
}
