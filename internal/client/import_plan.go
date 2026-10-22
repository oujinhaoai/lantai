package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/catalog/pathrule"
)

const (
	ImportInputSchema = "lantai.asset-import-input/v1"
	ImportPlanSchema  = "lantai.asset-import-plan/v1"
	// Limits apply to the complete walk, not a silently truncated selection.
	MaxImportFiles  = 30000
	MaxImportAssets = 1000
)

// ImportInput is an explicit local selection, not a server manifest or a scan
// rule. Directory is relative to the separately supplied source root ("." is
// allowed); Push uses the existing upload/commit document without new semantics.
type ImportInput struct {
	Schema   string            `json:"schema"`
	Assets   []ImportAsset     `json:"assets"`
	Excluded []ImportExclusion `json:"excluded,omitempty"`
}
type ImportAsset struct {
	Key            string    `json:"key"`
	Directory      string    `json:"directory"`
	Classification string    `json:"classification"`
	Push           PushInput `json:"push"`
}
type ImportExclusion struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}
type ImportDiscovery struct {
	Path           string `json:"path"`
	Kind           string `json:"kind"`
	Size           int64  `json:"size"`
	Classification string `json:"classification"`
}
type ImportPlan struct {
	Schema        string      `json:"schema"`
	Digest        string      `json:"digest"`
	SourceRoot    string      `json:"source_root"`
	Input         ImportInput `json:"input"`
	SelectedFiles int         `json:"selected_files"`
	SelectedBytes int64       `json:"selected_bytes"`
	// Uncovered is metadata only. Its presence never opts files into an upload.
	Uncovered []ImportDiscovery `json:"uncovered"`
}

// Strict local JSON decoding rejects misspelled fields instead of ignoring a
// selection, exclusion or safety constraint the operator thought was present.
func (in *ImportInput) UnmarshalJSON(b []byte) error {
	type plain ImportInput
	var value plain
	if err := importStrictJSON(b, &value); err != nil {
		return err
	}
	*in = ImportInput(value)
	return nil
}
func (p *ImportPlan) UnmarshalJSON(b []byte) error {
	type plain ImportPlan
	var value plain
	if err := importStrictJSON(b, &value); err != nil {
		return err
	}
	*p = ImportPlan(value)
	return nil
}
func importStrictJSON(b []byte, target any) error {
	if int64(len(b)) > MaxJSONBytes {
		return errors.New("client: import JSON exceeds local document limit")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("client: import JSON must contain one document")
	}
	return nil
}

// PrepareImport only reads the chosen source. It hashes every selected file and
// reports unselected entries without reading their payloads or following links.
func PrepareImport(ctx context.Context, input ImportInput, sourceRoot string) (ImportPlan, error) {
	var p ImportPlan
	if input.Schema != ImportInputSchema || len(input.Assets) == 0 || len(input.Assets) > MaxImportAssets {
		return p, errors.New("client: invalid import schema or asset count")
	}
	root, err := importAbsolute(sourceRoot)
	if err != nil {
		return p, err
	}
	if err = importNoLinks(root, false); err != nil {
		return p, err
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return p, errors.New("client: import source must be a directory")
	}
	// Detach all slices and RawMessages from the caller before freezing hashes.
	raw, err := json.Marshal(input)
	if err != nil || int64(len(raw)) > MaxJSONBytes {
		return p, errors.New("client: import input exceeds local document limit")
	}
	if err = json.Unmarshal(raw, &input); err != nil {
		return p, err
	}
	p = ImportPlan{Schema: ImportPlanSchema, SourceRoot: root, Input: input, Uncovered: []ImportDiscovery{}}
	keys := make([]string, 0, len(input.Assets))
	selected := map[string]bool{}
	totalSelected := 0
	for _, a := range input.Assets {
		if len(a.Push.Content.Files) > MaxImportFiles-totalSelected {
			return ImportPlan{}, errors.New("client: import selected file limit exceeded")
		}
		if len(a.Push.Content.Files) > MaxWorksetFiles {
			return ImportPlan{}, errors.New("client: import exceeds per-asset verification limit; split the selected asset explicitly")
		}
		totalSelected += len(a.Push.Content.Files)
	}
	for i := range p.Input.Assets {
		a := &p.Input.Assets[i]
		if err = SafeRelative(a.Key); err != nil || strings.Contains(a.Key, "/") || strings.HasPrefix(a.Key, ".") || len(a.Key) > 100 {
			return ImportPlan{}, errors.New("client: import asset key must be a safe single path component")
		}
		keys = append(keys, a.Key)
		if a.Directory != "." {
			if err = SafeRelative(a.Directory); err != nil {
				return ImportPlan{}, err
			}
		}
		if a.Classification != "final" && a.Classification != "intermediate" && a.Classification != "unknown" {
			return ImportPlan{}, errors.New("client: import classification must be final, intermediate or unknown")
		}
		if !manifest.AssetType(a.Push.Content.AssetType).Valid() {
			return ImportPlan{}, errors.New("client: unknown import asset type")
		}
		dir := filepath.Join(root, filepath.FromSlash(a.Directory))
		if err = importNoLinks(dir, false); err != nil {
			return ImportPlan{}, err
		}
		for _, f := range a.Push.Content.Files {
			if !slices.Contains(manifest.Roles, f.Role) {
				return ImportPlan{}, errors.New("client: unknown import file role")
			}
			if err = SafeRelative(f.Path); err != nil {
				return ImportPlan{}, err
			}
			rel := path.Join(a.Directory, f.Path)
			if selected[rel] {
				return ImportPlan{}, errors.New("client: source file selected by more than one asset")
			}
			if importCredentialPath(rel) {
				return ImportPlan{}, errors.New("client: credential-named input must be explicitly excluded")
			}
			if err = importNoLinks(filepath.Join(root, filepath.FromSlash(rel)), false); err != nil {
				return ImportPlan{}, err
			}
			st, statErr := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
			if statErr != nil {
				return ImportPlan{}, statErr
			}
			if !st.Mode().IsRegular() {
				return ImportPlan{}, errors.New("client: selected import source must be a regular file")
			}
			selected[rel] = true
			if len(selected) > MaxImportFiles {
				return ImportPlan{}, errors.New("client: import selected file limit exceeded")
			}
		}
		a.Push, err = PreparePush(ctx, a.Push, dir)
		if err != nil {
			return ImportPlan{}, err
		}
		var assetBytes int64
		for _, f := range a.Push.Content.Files {
			if f.Size > MaxWorksetBytes-assetBytes {
				return ImportPlan{}, errors.New("client: import exceeds per-asset verification byte limit")
			}
			assetBytes += f.Size
			if f.Size > int64(^uint64(0)>>1)-p.SelectedBytes {
				return ImportPlan{}, errors.New("client: import byte count overflow")
			}
			p.SelectedFiles++
			p.SelectedBytes += f.Size
		}
	}
	if err = ValidatePaths(keys); err != nil {
		return ImportPlan{}, err
	}
	allPaths := make([]string, 0, len(selected))
	for rel := range selected {
		allPaths = append(allPaths, rel)
	}
	if err = ValidatePaths(allPaths); err != nil {
		return ImportPlan{}, err
	}
	excluded := make([]string, 0, len(input.Excluded))
	for _, x := range input.Excluded {
		if SafeRelative(x.Path) != nil || strings.TrimSpace(x.Reason) == "" || len(x.Reason) > 2000 {
			return ImportPlan{}, errors.New("client: import exclusions need a safe path and a bounded reason")
		}
		full := filepath.Join(root, filepath.FromSlash(x.Path))
		// Excluding a symlink itself is allowed; escaping through its parent is not.
		if err = importNoLinks(filepath.Dir(full), false); err != nil {
			return ImportPlan{}, err
		}
		if _, err = os.Lstat(full); err != nil {
			return ImportPlan{}, err
		}
		for rel := range selected {
			if rel == x.Path || strings.HasPrefix(rel, x.Path+"/") {
				return ImportPlan{}, errors.New("client: import selection overlaps an exclusion")
			}
		}
		excluded = append(excluded, x.Path)
	}
	if err = ValidatePaths(excluded); err != nil {
		return ImportPlan{}, err
	}
	entries := 0
	observedSelected := map[string]bool{}
	observedExclusions := map[string]bool{}
	err = filepath.WalkDir(root, func(full string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if full == root {
			return nil
		}
		entries++
		if entries > MaxImportFiles {
			return errors.New("client: import discovery entry limit exceeded; narrow the source root")
		}
		rel, err := filepath.Rel(root, full)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		for _, x := range excluded {
			if rel == x || strings.HasPrefix(rel, x+"/") {
				observedExclusions[x] = true
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if selected[rel] {
			observedSelected[rel] = true
			return nil
		}
		if d.IsDir() {
			return nil
		}
		st, err := d.Info()
		if err != nil {
			return err
		}
		kind := "special"
		if st.Mode().IsRegular() {
			kind = "file"
		} else if st.Mode()&os.ModeSymlink != 0 {
			kind = "symlink"
		}
		p.Uncovered = append(p.Uncovered, ImportDiscovery{Path: rel, Kind: kind, Size: st.Size(), Classification: "unknown"})
		return nil
	})
	if err != nil {
		return ImportPlan{}, err
	}
	if len(observedSelected) != len(selected) || len(observedExclusions) != len(excluded) {
		return ImportPlan{}, errors.New("client: selection/exclusion spelling must exactly match source paths")
	}
	sort.Slice(p.Uncovered, func(i, j int) bool { return p.Uncovered[i].Path < p.Uncovered[j].Path })
	p.Digest, err = importPlanDigest(p)
	return p, err
}

// ValidateImportPlan refuses changed bytes, classification, selection, exclusions
// or discovery metadata. A new plan requires a new explicit review/apply action.
func ValidateImportPlan(ctx context.Context, plan ImportPlan, sourceRoot string) error {
	if plan.Schema != ImportPlanSchema {
		return errors.New("client: invalid import plan schema")
	}
	digest, err := importPlanDigest(plan)
	if err != nil || digest != plan.Digest {
		return errors.New("client: import plan digest mismatch")
	}
	fresh, err := PrepareImport(ctx, plan.Input, sourceRoot)
	if err != nil {
		return err
	}
	if fresh.Digest != plan.Digest {
		return errors.New("client: import source or plan changed; review a new plan")
	}
	return nil
}

func importPlanDigest(p ImportPlan) (string, error) {
	p.Digest = ""
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	if int64(len(b)) > MaxJSONBytes {
		return "", errors.New("client: import plan exceeds local document limit")
	}
	return importSHA(b), nil
}
func importSHA(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func importAbsolute(name string) (string, error) {
	if name == "" {
		return "", errors.New("client: explicit directory required")
	}
	abs, err := filepath.Abs(filepath.Clean(name))
	if err != nil {
		return "", err
	}
	if st, e := os.Lstat(abs); e == nil && st.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("client: import root must not be a symbolic link")
	}
	// Canonicalize existing ancestors, including macOS /var and /tmp aliases,
	// before overlap checks. Never reinterpret links beneath the selected root.
	ancestor := abs
	var tail []string
	for {
		_, e := os.Lstat(ancestor)
		if e == nil {
			break
		}
		if !errors.Is(e, os.ErrNotExist) {
			return "", e
		}
		tail = append(tail, filepath.Base(ancestor))
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", e
		}
		ancestor = parent
	}
	resolved, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return "", err
	}
	for i := len(tail) - 1; i >= 0; i-- {
		resolved = filepath.Join(resolved, tail[i])
	}
	return resolved, nil
}

// All existing path components are inspected. This deliberately rejects even
// in-root links rather than interpreting a changing link as source identity.
func importNoLinks(full string, allowMissing bool) error {
	volume := filepath.VolumeName(full)
	cur := volume + string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(full, cur), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		cur = filepath.Join(cur, component)
		st, err := os.Lstat(cur)
		if allowMissing && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return errors.New("client: import paths must not traverse symbolic links")
		}
	}
	return nil
}
func importCredentialPath(rel string) bool {
	parts := strings.Split(strings.ToLower(rel), "/")
	for _, part := range parts {
		if part == ".ssh" || part == ".aws" || part == ".kube" || part == ".gnupg" {
			return true
		}
	}
	name := parts[len(parts)-1]
	return name == ".env" || strings.HasPrefix(name, ".env.") || name == "credentials" || strings.HasPrefix(name, "credentials.") || name == "token.json" || name == "session.json" || name == "auth.json" || name == "secrets" || strings.HasPrefix(name, "secrets.") || name == "id_rsa" || name == "id_ed25519" || strings.HasSuffix(name, ".key") || strings.HasSuffix(name, ".pem")
}
func importDisjoint(dirs ...string) error {
	for i, a := range dirs {
		for _, b := range dirs[i+1:] {
			// EvalSymlinks preserves spelling on case-insensitive volumes. Use
			// the same portable Unicode/case identity as manifest paths so a
			// Source/SOURCE alias cannot create state inside the original source.
			aKey, bKey := pathrule.Key(a), pathrule.Key(b)
			rel, err := filepath.Rel(aKey, bKey)
			back, e := filepath.Rel(bKey, aKey)
			inside := func(r string) bool {
				return r == "." || (r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) && !filepath.IsAbs(r))
			}
			if err != nil || e != nil || inside(rel) || inside(back) {
				return errors.New("client: import source, state and verification roots must not overlap")
			}
		}
	}
	return nil
}
func importPrivateDirectory(dir string) error {
	if err := importNoLinks(dir, true); err != nil {
		return err
	}
	if err := makeImportPrivateDirectory(dir); err != nil {
		return err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return errors.New("client: import state must be a private directory")
	}
	if err = checkPrivate(dir, st); err != nil {
		return fmt.Errorf("client: import state directory must be private: %w", err)
	}
	return nil
}
