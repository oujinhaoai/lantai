package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/oujinhaoai/lantai/internal/catalog/pathrule"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

const (
	WorksetSchema        = "lantai.client-workset/v1"
	WorksetPlanSchema    = "lantai.client-workset-plan/v1"
	WorksetReceiptSchema = "lantai.client-workset-receipt/v1"
	MaxWorksetEntries    = 1000
	MaxWorksetFiles      = 10000
	// MaxWorksetBytes limits a single working copy to 1 TiB, without buffering
	// file content. Larger collections must be split into explicit worksets.
	MaxWorksetBytes int64 = 1 << 40
)

// WorksetInput is a local working-copy instruction, not a server manifest or
// permission grant. Every entry names an exact immutable version.
type WorksetInput struct {
	Schema       string              `json:"schema"`
	Entries      []WorksetEntry      `json:"entries"`
	Entrypoints  []string            `json:"entrypoints,omitempty"`
	Dependencies []WorksetDependency `json:"dependencies,omitempty"`
}

type WorksetEntry struct {
	AssetID     string `json:"asset_id"`
	VersionID   string `json:"version_id"`
	Destination string `json:"destination"`
}

// WorksetDependency states required file paths inside this working copy. It
// never causes automatic resolution of another asset or execution of a file.
type WorksetDependency struct {
	From     string   `json:"from"`
	Requires []string `json:"requires"`
}

type WorksetFile struct {
	SourcePath string `json:"source_path"`
	Path       string `json:"path"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
	State      string `json:"state"` // planned, present, verified
}

type WorksetPlannedEntry struct {
	WorksetEntry
	InstanceID     string        `json:"instance_id"`
	ManifestDigest digest.Digest `json:"manifest_digest"`
	Files          []WorksetFile `json:"files"`
}

type WorksetPlan struct {
	Schema       string                `json:"schema"`
	Origin       string                `json:"origin"`
	InputDigest  digest.Digest         `json:"input_digest"`
	State        string                `json:"state"` // planned, partial, completed
	FileCount    int                   `json:"file_count"`
	TotalBytes   int64                 `json:"total_bytes"`
	Entries      []WorksetPlannedEntry `json:"entries"`
	Entrypoints  []string              `json:"entrypoints,omitempty"`
	Dependencies []WorksetDependency   `json:"dependencies,omitempty"`
}

// WorksetReceipt is returned even on a failed pull; only completed means that
// every file was checked against the pinned manifest. Callers may save it, but
// it contains no credentials or grant URLs and never substitutes for rechecking.
type WorksetReceipt WorksetPlan

// DecodeWorkset rejects unknown fields, trailing documents and oversized input.
func DecodeWorkset(r io.Reader) (WorksetInput, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxJSONBytes+1))
	if err != nil {
		return WorksetInput{}, err
	}
	if int64(len(raw)) > MaxJSONBytes {
		return WorksetInput{}, worksetInvalid("workset input exceeds 8 MiB")
	}
	if _, err = canonjson.Canonicalize(raw); err != nil {
		return WorksetInput{}, err
	}
	var in WorksetInput
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(&in); err != nil {
		return in, err
	}
	if err = d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return in, worksetInvalid("workset must contain exactly one JSON document")
	}
	return in, nil
}

// ValidateWorkset performs local preflight only. It creates no directory and
// sends no request. Destination roots and all existing descendants must not be
// symlinks; each entry owns a distinct, non-overlapping relative directory.
func ValidateWorkset(in WorksetInput, directory string) error {
	if in.Schema != WorksetSchema || len(in.Entries) == 0 || len(in.Entries) > MaxWorksetEntries {
		return worksetInvalid("workset schema and 1..1000 entries are required")
	}
	raw, err := json.Marshal(in)
	if err != nil || int64(len(raw)) > MaxJSONBytes {
		return worksetInvalid("invalid or oversized workset input")
	}
	destinations := make([]string, len(in.Entries))
	for i, e := range in.Entries {
		if !ids.ID(e.AssetID).Valid() || !ids.ID(e.VersionID).Valid() {
			return worksetInvalid("workset entries require canonical asset_id and version_id")
		}
		if err = worksetPath(e.Destination); err != nil {
			return err
		}
		destinations[i] = e.Destination
	}
	if worksetConflicts(destinations) {
		return worksetConflict("workset destination directories overlap or collide")
	}
	for _, p := range in.Entrypoints {
		if err = worksetPath(p); err != nil {
			return err
		}
	}
	for _, d := range in.Dependencies {
		if err = worksetPath(d.From); err != nil {
			return err
		}
		if len(d.Requires) == 0 {
			return worksetInvalid("a dependency declaration must contain required paths")
		}
		for _, p := range d.Requires {
			if err = worksetPath(p); err != nil {
				return err
			}
		}
	}
	root, err := openWorksetRoot(directory, false)
	if err != nil || root == nil {
		return err
	}
	defer root.Close()
	for _, p := range destinations {
		if err = checkWorksetTarget(root, p, true); err != nil {
			return err
		}
	}
	return nil
}

// PlanWorkset reads server metadata and exact manifests only. All manifests, target collisions,
// declared dependencies and existing files are verified before any write or
// read grant. A partially built plan is returned on error for diagnosis only.
func (c *Client) PlanWorkset(ctx context.Context, in WorksetInput, directory string) (WorksetPlan, error) {
	if err := ValidateWorkset(in, directory); err != nil {
		return WorksetPlan{}, err
	}
	raw, _ := json.Marshal(in)
	canonical, err := canonjson.Canonicalize(raw)
	if err != nil {
		return WorksetPlan{}, err
	}
	p := WorksetPlan{Schema: WorksetPlanSchema, Origin: c.Origin(), InputDigest: digest.Of(canonical),
		State: "planned", Entrypoints: append([]string(nil), in.Entrypoints...), Dependencies: in.Dependencies}
	instanceID, err := c.worksetInstance(ctx)
	if err != nil {
		return p, err
	}
	var paths []string
	for _, e := range in.Entries {
		entry, err := c.worksetManifest(ctx, e, instanceID)
		if err != nil {
			return p, err
		}
		for _, f := range entry.Files {
			p.FileCount++
			if p.FileCount > MaxWorksetFiles || f.Size > MaxWorksetBytes-p.TotalBytes {
				return p, worksetInvalid("workset exceeds 10000 files or 1 TiB; split it into smaller worksets")
			}
			p.TotalBytes += f.Size
			paths = append(paths, f.Path)
		}
		p.Entries = append(p.Entries, entry)
	}
	if worksetConflicts(paths) {
		return p, worksetConflict("workset file destinations collide or conflict with a directory")
	}
	if err = checkWorksetDependencies(in, paths); err != nil {
		return p, err
	}
	root, err := openWorksetRoot(directory, false)
	if err != nil {
		return p, err
	}
	if root != nil {
		defer root.Close()
		if err = checkWorksetFiles(ctx, root, &p); err != nil {
			return p, err
		}
	}
	encoded, err := json.Marshal(p)
	// A completed receipt uses a longer schema/state and "verified" for each
	// file. Reserve that growth now, before any output file can be written.
	receiptGrowth := p.FileCount + len(WorksetReceiptSchema) - len(WorksetPlanSchema) + len("completed") - len("planned")
	if err != nil || int64(len(encoded)+receiptGrowth) > MaxJSONBytes {
		return p, worksetInvalid("workset plan exceeds 8 MiB; split it into smaller worksets")
	}
	return p, nil
}

// PullWorkset performs one complete preflight, then downloads through the same
// pinned os.Root for every entry. Existing different files are never replaced.
// Repeating the same input safely verifies completed files and resumes partials.
func (c *Client) PullWorkset(ctx context.Context, in WorksetInput, directory, purpose string) (WorksetReceipt, error) {
	p, err := c.PlanWorkset(ctx, in, directory)
	r := WorksetReceipt(p)
	r.Schema, r.State = WorksetReceiptSchema, "partial"
	if err != nil {
		return r, err
	}
	root, err := openWorksetRoot(directory, true)
	if err != nil {
		return r, err
	}
	defer root.Close()
	// Recheck every local target before the first grant, since the filesystem
	// may have changed while the read-only plan fetched the manifests.
	if err = checkWorksetFiles(ctx, root, &p); err != nil {
		return r, err
	}
	for i := range r.Entries {
		e := &r.Entries[i]
		for j := range e.Files {
			f := &e.Files[j]
			if err = checkWorksetRoot(directory, root); err != nil {
				return r, err
			}
			if err = checkWorksetTarget(root, f.Path, false); err != nil {
				return r, err
			}
			endpoint := "/api/v1/assets/" + e.AssetID + "/versions/" + e.VersionID + "/read-grants"
			res, err := c.Do(ctx, http.MethodPost, endpoint, map[string]string{"path": f.SourcePath, "purpose": purpose}, Options{})
			if err != nil {
				return r, err
			}
			var g DownloadGrant
			if err = res.Decode(&g); err != nil {
				return r, err
			}
			if g.Path != f.SourcePath || g.SHA256 != f.SHA256 || g.Size != f.Size {
				return r, errcode.New(errcode.RefMismatch, "client: read grant does not match the pinned workset manifest")
			}
			if err = checkWorksetRoot(directory, root); err != nil {
				return r, err
			}
			if err = checkWorksetTarget(root, f.Path, false); err != nil {
				return r, err
			}
			if err = c.downloadRoot(ctx, g, root, f.Path); err != nil {
				return r, err
			}
			if err = checkWorksetTarget(root, f.Path, false); err != nil {
				return r, err
			}
			f.State = "verified"
		}
	}
	if err = checkWorksetRoot(directory, root); err != nil {
		return r, err
	}
	r.State = "completed"
	return r, nil
}

// worksetInstance binds the plan to the instance advertised by this client's
// current origin, using the existing metadata API. A merely well-formed ID in
// an exact manifest is not evidence that it belongs to that same instance.
func (c *Client) worksetInstance(ctx context.Context) (string, error) {
	r, err := c.Do(ctx, http.MethodGet, "/api/v1/meta", nil, Options{})
	if err != nil {
		return "", err
	}
	if _, err = canonjson.Canonicalize(r.Body); err != nil {
		return "", worksetInvalid("invalid workset server metadata")
	}
	var meta struct {
		APIVersion string `json:"api_version"`
		InstanceID string `json:"instance_id"`
	}
	if err = r.Decode(&meta); err != nil || meta.APIVersion != "v1" || !ids.ID(meta.InstanceID).Valid() {
		return "", worksetInvalid("workset server metadata requires api_version v1 and a canonical instance_id")
	}
	return meta.InstanceID, nil
}

func (c *Client) worksetManifest(ctx context.Context, e WorksetEntry, instanceID string) (WorksetPlannedEntry, error) {
	out := WorksetPlannedEntry{WorksetEntry: e}
	endpoint := "/api/v1/assets/" + e.AssetID + "/versions/" + e.VersionID + "?view=full"
	r, err := c.Do(ctx, http.MethodGet, endpoint, nil, Options{})
	if err != nil {
		return out, err
	}
	var v struct {
		Version struct {
			AssetID        string        `json:"asset_id"`
			VersionID      string        `json:"version_id"`
			ManifestDigest digest.Digest `json:"manifest_digest"`
		} `json:"version"`
		Manifest *struct {
			InstanceID     string          `json:"instance_id"`
			AssetID        string          `json:"asset_id"`
			VersionID      string          `json:"version_id"`
			ManifestDigest digest.Digest   `json:"manifest_digest"`
			Content        json.RawMessage `json:"content"`
		} `json:"manifest"`
	}
	if err = r.Decode(&v); err != nil {
		return out, err
	}
	if v.Manifest == nil || v.Version.AssetID != e.AssetID || v.Version.VersionID != e.VersionID ||
		v.Manifest.AssetID != e.AssetID || v.Manifest.VersionID != e.VersionID || !ids.ID(v.Manifest.InstanceID).Valid() ||
		v.Manifest.InstanceID != instanceID ||
		!v.Manifest.ManifestDigest.Valid() || v.Version.ManifestDigest != v.Manifest.ManifestDigest {
		return out, errcode.New(errcode.RefMismatch, "client: exact workset version or manifest identity mismatch")
	}
	canonical, err := canonjson.Canonicalize(v.Manifest.Content)
	if err != nil || digest.Of(canonical) != v.Manifest.ManifestDigest {
		return out, errcode.New(errcode.HashMismatch, "client: exact workset manifest digest mismatch")
	}
	var content struct {
		Files []InputFile `json:"files"`
	}
	if err = json.Unmarshal(v.Manifest.Content, &content); err != nil {
		return out, err
	}
	if len(content.Files) == 0 || len(content.Files) > MaxWorksetFiles {
		return out, worksetInvalid("workset manifest must contain 1..10000 files")
	}
	out.InstanceID, out.ManifestDigest = v.Manifest.InstanceID, v.Manifest.ManifestDigest
	var sources []string
	for _, f := range content.Files {
		if err = worksetPath(f.Path); err != nil {
			return out, err
		}
		if !digest.ValidHex(f.SHA256) || f.Size < 0 {
			return out, worksetInvalid("invalid workset manifest file size or digest")
		}
		target := e.Destination + "/" + f.Path
		if err = worksetPath(target); err != nil {
			return out, err
		}
		sources = append(sources, f.Path)
		out.Files = append(out.Files, WorksetFile{SourcePath: f.Path, Path: target, SHA256: f.SHA256, Size: f.Size, State: "planned"})
	}
	if worksetConflicts(sources) {
		return out, worksetConflict("workset manifest paths collide")
	}
	return out, nil
}

func checkWorksetDependencies(in WorksetInput, paths []string) error {
	present := make(map[string]bool, len(paths))
	for _, p := range paths {
		present[p] = true
	}
	require := func(p string) error {
		if !present[p] {
			return worksetInvalid(fmt.Sprintf("workset required path is absent from the pinned manifests: %s", p))
		}
		return nil
	}
	for _, p := range in.Entrypoints {
		if err := require(p); err != nil {
			return err
		}
	}
	for _, d := range in.Dependencies {
		if err := require(d.From); err != nil {
			return err
		}
		for _, p := range d.Requires {
			if err := require(p); err != nil {
				return err
			}
		}
	}
	return nil
}

func worksetPath(p string) error {
	if err := pathrule.Check(p); err != nil {
		return err
	}
	return SafeRelative(p)
}

// Also reject two spellings of the same intermediate directory (A/x, a/y).
// Otherwise a plan can have a different directory layout on Linux and macOS.
func worksetConflicts(paths []string) bool {
	if len(pathrule.Conflicts(paths)) > 0 {
		return true
	}
	seen := map[string]string{}
	for _, p := range paths {
		parts := strings.Split(p, "/")
		for i := range parts {
			prefix := strings.Join(parts[:i+1], "/")
			key := pathrule.Key(prefix)
			if previous, ok := seen[key]; ok && previous != prefix {
				return true
			}
			seen[key] = prefix
		}
	}
	return false
}
func worksetInvalid(message string) error {
	return errcode.New(errcode.SchemaInvalid, "client: "+message)
}
func worksetConflict(message string) error {
	return errcode.New(errcode.PathConflict, "client: "+message)
}

// openWorksetRoot walks from a pinned filesystem root, never following a
// symbolic link (even when creating missing parents). Each opened child is
// compared with its directory entry. All subsequent writes use the final root.
func openWorksetRoot(directory string, create bool) (*os.Root, error) {
	if directory == "" {
		return nil, worksetInvalid("workset destination root is required")
	}
	abs, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	volume := filepath.VolumeName(abs)
	anchor := volume + string(filepath.Separator)
	if abs == anchor {
		return nil, worksetInvalid("workset destination must not be a filesystem root")
	}
	root, err := os.OpenRoot(anchor)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(abs, anchor), string(filepath.Separator)) {
		st, err := root.Lstat(part)
		if errors.Is(err, os.ErrNotExist) {
			if !create {
				root.Close()
				return nil, nil
			}
			err = root.Mkdir(part, 0700)
			if err != nil && !errors.Is(err, os.ErrExist) {
				root.Close()
				return nil, err
			}
			st, err = root.Lstat(part)
		}
		if err != nil {
			root.Close()
			return nil, err
		}
		if !st.IsDir() {
			root.Close()
			return nil, worksetConflict("workset destination contains a symlink or non-directory")
		}
		next, err := root.OpenRoot(part)
		if err != nil {
			root.Close()
			return nil, err
		}
		opened, openErr := next.Stat(".")
		current, statErr := root.Lstat(part)
		root.Close()
		if openErr != nil || statErr != nil || !current.IsDir() || !os.SameFile(st, opened) || !os.SameFile(opened, current) {
			next.Close()
			return nil, worksetConflict("workset destination changed while opening")
		}
		root = next
	}
	return root, nil
}

func checkWorksetTarget(root *os.Root, target string, directory bool) error {
	parts := strings.Split(target, "/")
	for i, name := range parts {
		parent := "."
		if i > 0 {
			parent = strings.Join(parts[:i], "/")
		}
		if err := checkWorksetSpelling(root, parent, name); err != nil {
			return err
		}
		st, err := root.Lstat(filepath.FromSlash(strings.Join(parts[:i+1], "/")))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		wantDir := i < len(parts)-1 || directory
		if (wantDir && !st.IsDir()) || (!wantDir && !st.Mode().IsRegular()) {
			return worksetConflict("workset target contains a symlink or incompatible file type")
		}
	}
	return nil
}

func checkWorksetSpelling(root *os.Root, parent, name string) error {
	d, err := root.Open(filepath.FromSlash(parent))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer d.Close()
	for {
		names, err := d.Readdirnames(256)
		for _, existing := range names {
			if existing != name && pathrule.Key(existing) == pathrule.Key(name) {
				return worksetConflict("workset target collides with an existing name after Unicode/case folding")
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func checkWorksetFiles(ctx context.Context, root *os.Root, p *WorksetPlan) error {
	for i := range p.Entries {
		for j := range p.Entries[i].Files {
			f := &p.Entries[i].Files[j]
			if err := checkWorksetTarget(root, f.Path, false); err != nil {
				return err
			}
			for _, suffix := range []string{".lantai-part", ".lantai-download.json", ".lantai-lock"} {
				if err := checkWorksetTarget(root, f.Path+suffix, false); err != nil {
					return err
				}
			}
			file, err := root.Open(filepath.FromSlash(f.Path))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			same, err := checkFile(ctx, file, f.SHA256, f.Size)
			file.Close()
			if err != nil {
				return err
			}
			if !same {
				return worksetConflict("workset destination exists with different content; preserve it and select an empty working copy")
			}
			f.State = "present"
		}
	}
	return nil
}

func checkWorksetRoot(directory string, pinned *os.Root) error {
	current, err := openWorksetRoot(directory, false)
	if err != nil {
		return err
	}
	if current == nil {
		return worksetConflict("workset destination root disappeared")
	}
	defer current.Close()
	a, err := current.Stat(".")
	if err != nil {
		return err
	}
	b, err := pinned.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(a, b) {
		return worksetConflict("workset destination root was replaced")
	}
	return nil
}
