package client

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type ImportVerifyResult struct {
	Key       string `json:"key"`
	Status    string `json:"status"`
	Files     int    `json:"files"`
	Bytes     int64  `json:"bytes"`
	ErrorCode string `json:"error_code,omitempty"`
}
type ImportVerifyReport struct {
	Schema                string               `json:"schema"`
	Origin                string               `json:"origin"`
	PlanDigest            string               `json:"plan_digest"`
	Complete              bool                 `json:"complete"`
	CompletionBasis       string               `json:"completion_basis"`
	CurrentRemoteVerified bool                 `json:"current_remote_verified"`
	Results               []ImportVerifyResult `json:"results"`
}
type importVerifyBinding struct {
	Schema     string `json:"schema"`
	Origin     string `json:"origin"`
	PlanDigest string `json:"plan_digest"`
}

// VerifyImport reads only frozen version identities and downloads into a new,
// independent root. The local state/report must agree; a caller-edited report
// cannot redirect verification to unrelated versions. A matching marker enables
// retry with the original per-file Download resume state, never an overwrite.
func (c *Client) VerifyImport(ctx context.Context, plan ImportPlan, report ImportReport, sourceRoot, stateDirectory, destination, purpose string) (ImportVerifyReport, error) {
	out := ImportVerifyReport{Schema: "lantai.asset-import-verify/v1", Origin: c.Origin(), PlanDigest: plan.Digest, CompletionBasis: "current_remote_and_local_files", Results: []ImportVerifyResult{}}
	if purpose == "" {
		return out, errors.New("client: explicit verification purpose required")
	}
	if err := ValidateImportPlan(ctx, plan, sourceRoot); err != nil {
		return out, err
	}
	if err := c.validateImportReport(plan, report); err != nil {
		return out, err
	}
	stateDir, err := importAbsolute(stateDirectory)
	if err != nil {
		return out, err
	}
	target, err := importAbsolute(destination)
	if err != nil {
		return out, err
	}
	if err = importDisjoint(plan.SourceRoot, stateDir, target); err != nil {
		return out, err
	}
	if err = importPrivateDirectory(stateDir); err != nil {
		return out, err
	}
	statePath := filepath.Join(stateDir, "import.json")
	unlock, err := lockState(statePath)
	if err != nil {
		return out, err
	}
	defer unlock()
	var saved ImportReport
	if err = LoadState(statePath, &saved); err != nil {
		return out, err
	}
	a, _ := json.Marshal(saved)
	b, _ := json.Marshal(report)
	if string(a) != string(b) {
		return out, errors.New("client: verification report differs from original import state")
	}
	if err = importNoLinks(target, true); err != nil {
		return out, err
	}
	if err = os.MkdirAll(target, 0700); err != nil {
		return out, err
	}
	marker := filepath.Join(target, ".lantai-import-verify.json")
	wanted := importVerifyBinding{Schema: "lantai.asset-import-verify-binding/v1", Origin: c.Origin(), PlanDigest: plan.Digest}
	var bound importVerifyBinding
	if err = LoadState(marker, &bound); errors.Is(err, os.ErrNotExist) {
		entries, e := os.ReadDir(target)
		if e != nil {
			return out, e
		}
		if len(entries) != 0 {
			return out, errors.New("client: verification destination must be empty or bound to the original plan")
		}
		if err = SaveState(marker, wanted); err != nil {
			return out, err
		}
	} else if err != nil {
		return out, err
	} else if bound != wanted {
		return out, errors.New("client: verification destination belongs to another plan or server")
	}
	// Check the whole destination, including stale extra files and symlinks,
	// before initiating downloads. Transfer artifacts are only allowed for an
	// expected file path in this bound root.
	expected := map[string]InputFile{}
	for i, item := range plan.Input.Assets {
		if !importCommitted(report.Results[i].Status) {
			continue
		}
		for _, f := range item.Push.Content.Files {
			expected[item.Key+"/"+f.Path] = f
		}
	}
	if err = importCheckDestination(ctx, target, expected, false); err != nil {
		return out, err
	}
	var failures []error
	for i, item := range plan.Input.Assets {
		r := report.Results[i]
		vr := ImportVerifyResult{Key: item.Key, Status: "not_committed"}
		if !importCommitted(r.Status) {
			out.Results = append(out.Results, vr)
			failures = append(failures, errors.New("client: cannot verify an import item without a committed receipt"))
			continue
		}
		err = c.importCheckManifest(ctx, item, r)
		if err == nil {
			// The workset implementation pins the complete target root, verifies
			// instance/asset/version/content digest and rechecks root identity
			// around every read grant and transfer. Do not grow a second download
			// implementation here or pin only the item's replaceable subtree.
			workset := WorksetInput{Schema: WorksetSchema, Entries: []WorksetEntry{{AssetID: r.AssetID, VersionID: r.VersionID, Destination: item.Key}}}
			_, err = c.PullWorkset(ctx, workset, target, purpose)
		}
		if err == nil {
			files := map[string]InputFile{}
			for _, f := range item.Push.Content.Files {
				files[f.Path] = f
			}
			err = importCheckDestination(ctx, filepath.Join(target, item.Key), files, true)
		}
		if err != nil {
			vr.Status = "failed"
			vr.ErrorCode = importErrorCode(err)
			failures = append(failures, errors.New("client: import verification failed for "+item.Key+"; preserve source and reuse verification destination"))
		} else {
			vr.Status = "verified"
			for _, f := range item.Push.Content.Files {
				vr.Files++
				vr.Bytes += f.Size
			}
		}
		out.Results = append(out.Results, vr)
	}
	out.Complete = len(failures) == 0 && len(out.Results) == len(plan.Input.Assets)
	out.CurrentRemoteVerified = out.Complete
	return out, errors.Join(failures...)
}

func (c *Client) importCheckManifest(ctx context.Context, item ImportAsset, result ImportResult) error {
	endpoint := "/api/v1/assets/" + url.PathEscape(result.AssetID) + "/versions/" + url.PathEscape(result.VersionID) + "?view=full"
	r, err := c.Do(ctx, http.MethodGet, endpoint, nil, Options{})
	if err != nil {
		return err
	}
	var version struct {
		Manifest *struct {
			Content struct {
				Files []InputFile `json:"files"`
			} `json:"content"`
		} `json:"manifest"`
	}
	if err = r.Decode(&version); err != nil {
		return err
	}
	if version.Manifest == nil || len(version.Manifest.Content.Files) != len(item.Push.Content.Files) {
		return errors.New("client: exact version file count differs from import plan")
	}
	expected := map[string]InputFile{}
	for _, f := range item.Push.Content.Files {
		expected[f.Path] = f
	}
	seen := map[string]bool{}
	for _, f := range version.Manifest.Content.Files {
		want, ok := expected[f.Path]
		if !ok || seen[f.Path] || want != f {
			return errors.New("client: exact version manifest differs from import plan")
		}
		seen[f.Path] = true
	}
	return nil
}

func importCheckDestination(ctx context.Context, root string, expected map[string]InputFile, requireAll bool) error {
	found := map[string]bool{}
	err := filepath.WalkDir(root, func(full string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if full == root {
			return nil
		}
		rel, e := filepath.Rel(root, full)
		if e != nil {
			return e
		}
		rel = filepath.ToSlash(rel)
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("client: verification destination contains a symbolic link")
		}
		if d.IsDir() {
			for name := range expected {
				if strings.HasPrefix(name, rel+"/") {
					return nil
				}
			}
			return errors.New("client: verification destination contains an unexpected directory")
		}
		if rel == ".lantai-import-verify.json" && !requireAll {
			return nil
		}
		f, ok := expected[rel]
		if !ok {
			for _, suffix := range []string{".lantai-part", ".lantai-download.json", ".lantai-lock"} {
				if strings.HasSuffix(rel, suffix) {
					if _, known := expected[strings.TrimSuffix(rel, suffix)]; known {
						st, err := d.Info()
						if err != nil {
							return err
						}
						if !st.Mode().IsRegular() {
							return errors.New("client: non-regular verification transfer state")
						}
						if requireAll && suffix != ".lantai-lock" {
							return errors.New("client: verification has unfinished transfer files")
						}
						return nil
					}
				}
			}
			return errors.New("client: verification destination contains an unexpected file")
		}
		if err := importNoLinks(full, false); err != nil {
			return err
		}
		st, e := d.Info()
		if e != nil {
			return e
		}
		if !st.Mode().IsRegular() {
			return errors.New("client: verification payload must be a regular file")
		}
		digest, size, e := HashFile(ctx, full)
		if e != nil {
			return e
		}
		if digest != f.SHA256 || size != f.Size {
			return errors.New("client: verification destination has conflicting bytes")
		}
		found[rel] = true
		return nil
	})
	if err != nil {
		return err
	}
	if requireAll && len(found) != len(expected) {
		return errors.New("client: verification file count differs from import plan")
	}
	return nil
}
