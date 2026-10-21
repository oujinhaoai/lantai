package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

const ReferenceReceiptSchema = "lantai.reference-receipt/v1"
const referenceStateName = ".lantai-reference-state.json"
const referenceLockName = ".lantai-reference-lock"
const referencePartSuffix = ".lantai-reference-part"

type ReferenceResult struct {
	Path         string `json:"path"`
	State        string `json:"state"`
	BeforeSHA256 string `json:"before_sha256"`
	AfterSHA256  string `json:"after_sha256"`
	OutputSHA256 string `json:"output_sha256,omitempty"`
	ErrorCode    string `json:"error_code,omitempty"`
}
type ReferenceReceipt struct {
	Schema          string            `json:"schema"`
	PlanDigest      string            `json:"plan_digest"`
	Mode            string            `json:"mode"`
	SourceRoot      string            `json:"source_root"`
	Destination     string            `json:"destination"`
	Complete        bool              `json:"complete"`
	Scope           string            `json:"scope"`
	Results         []ReferenceResult `json:"results"`
	PostCutoverRefs []string          `json:"post_cutover_refs"`
	InventoryScope  string            `json:"inventory_scope"`
}
type referenceBinding struct {
	Schema          string   `json:"schema"`
	PlanDigest      string   `json:"plan_digest"`
	Mode            string   `json:"mode"`
	FromRoot        string   `json:"from_root"`
	PostCutoverRefs []string `json:"post_cutover_refs"`
}

// CopyReferenceNotes only writes selected transformed Markdown into a separate
// private root. Source notes and server state are never changed. An existing
// bound root can resume only while every final/partial byte still matches.
func CopyReferenceNotes(ctx context.Context, p ReferencePlan, sourceRoot, destination string) (ReferenceReceipt, error) {
	r := referenceReceipt(p, "copy", sourceRoot, destination, []string{})
	if err := ValidateReferencePlan(ctx, p, sourceRoot); err != nil {
		return r, err
	}
	return referenceCopy(ctx, p, p.SourceRoot, destination, "copy", []string{})
}

// RollbackReferenceNotes creates a third, independent copy with the original
// links. It never rewrites the source vault, the edited copy, or server versions.
// postCutoverRefs is an explicit caller inventory, preserved verbatim in order;
// no server query or claim of whole-library enumeration is made.
func RollbackReferenceNotes(ctx context.Context, p ReferencePlan, copyRoot, destination string, postCutoverRefs []string) (ReferenceReceipt, error) {
	r := referenceReceipt(p, "rollback", copyRoot, destination, postCutoverRefs)
	if err := validateReferenceSnapshot(p); err != nil {
		return r, err
	}
	if postCutoverRefs == nil || len(postCutoverRefs) > 10000 {
		return r, errors.New("client: explicit post_cutover_refs array (possibly empty) is required")
	}
	for _, ref := range postCutoverRefs {
		if _, err := ids.ParseURI(ref); err != nil {
			return r, err
		}
	}
	from, err := importAbsolute(copyRoot)
	if err != nil {
		return r, err
	}
	root, err := openWorksetRoot(from, false)
	if err != nil {
		return r, err
	}
	if root == nil {
		return r, os.ErrNotExist
	}
	defer root.Close()
	st, err := root.Stat(".")
	if err != nil {
		return r, err
	}
	if err = checkPrivate(from, st); err != nil {
		return r, err
	}
	var binding referenceBinding
	if err = readReferenceState(root, referenceStateName, &binding); err != nil {
		return r, err
	}
	want := referenceBinding{Schema: "lantai.reference-binding/v1", PlanDigest: p.Digest, Mode: "copy", FromRoot: p.SourceRoot, PostCutoverRefs: []string{}}
	if !referenceBindingsEqual(binding, want) {
		return r, errors.New("client: rollback input is not the original bound reference copy")
	}
	if err = scanReferenceOutput(ctx, from, root, p, "copy", true); err != nil {
		return r, err
	}
	return referenceCopy(ctx, p, from, destination, "rollback", append([]string{}, postCutoverRefs...))
}
func referenceReceipt(p ReferencePlan, mode, source, dest string, refs []string) ReferenceReceipt {
	r := ReferenceReceipt{Schema: ReferenceReceiptSchema, PlanDigest: p.Digest, Mode: mode, SourceRoot: source, Destination: dest, Scope: "explicit_mappings_in_selected_notes_only", Results: []ReferenceResult{}, PostCutoverRefs: append([]string{}, refs...), InventoryScope: "caller_declared_only_not_whole_library"}
	for _, n := range p.Notes {
		r.Results = append(r.Results, ReferenceResult{Path: n.Path, State: "not_attempted", BeforeSHA256: n.BeforeSHA256, AfterSHA256: n.AfterSHA256})
	}
	return r
}
func referenceCopy(ctx context.Context, p ReferencePlan, sourceRoot, destination, mode string, refs []string) (ReferenceReceipt, error) {
	r := referenceReceipt(p, mode, sourceRoot, destination, refs)
	from, err := importAbsolute(sourceRoot)
	if err != nil {
		return r, err
	}
	target, err := importAbsolute(destination)
	if err != nil {
		return r, err
	}
	r.SourceRoot, r.Destination = from, target
	if mode == "copy" {
		err = importDisjoint(from, target)
	} else {
		err = importDisjoint(p.SourceRoot, from, target)
	}
	if err != nil {
		return r, err
	}
	source, err := openWorksetRoot(from, false)
	if err != nil {
		return r, err
	}
	if source == nil {
		return r, os.ErrNotExist
	}
	defer source.Close()
	// Check all source bytes before creating output; recheck each before its
	// write and once more before declaring completion to detect concurrent edits.
	checkSource := func(n ReferenceNotePlan) error {
		b, err := readReferenceNote(ctx, from, source, n.Path)
		if err != nil {
			return err
		}
		want := n.BeforeSHA256
		if mode == "rollback" {
			want = n.AfterSHA256
		}
		if importSHA(b) != want {
			return errors.New("client: reference source changed; preserve both copies and review a new plan")
		}
		return nil
	}
	for _, n := range p.Notes {
		if err = checkSource(n); err != nil {
			return r, err
		}
	}
	if err = importPrivateDirectory(target); err != nil {
		return r, err
	}
	root, err := openWorksetRoot(target, false)
	if err != nil {
		return r, err
	}
	if root == nil {
		return r, os.ErrNotExist
	}
	defer root.Close()
	wanted := referenceBinding{Schema: "lantai.reference-binding/v1", PlanDigest: p.Digest, Mode: mode, FromRoot: from, PostCutoverRefs: append([]string{}, refs...)}
	if err = preflightReferenceDestination(root, wanted); err != nil {
		return r, err
	}
	unlock, err := lockReferenceRoot(root)
	if err != nil {
		return r, err
	}
	defer unlock()
	var binding referenceBinding
	if err = readReferenceState(root, referenceStateName, &binding); errors.Is(err, os.ErrNotExist) {
		d, err := root.Open(".")
		if err != nil {
			return r, err
		}
		names, err := d.Readdirnames(-1)
		d.Close()
		if err != nil {
			return r, err
		}
		for _, name := range names {
			if name != referenceLockName {
				return r, errors.New("client: reference destination must be empty or bound to the same plan and mode")
			}
		}
		if err = checkWorksetRoot(target, root); err != nil {
			return r, err
		}
		if err = writeReferenceState(root, referenceStateName, wanted); err != nil {
			return r, err
		}
	} else if err != nil {
		return r, err
	} else if !referenceBindingsEqual(binding, wanted) {
		return r, errors.New("client: reference destination belongs to another plan, mode or declared inventory")
	}
	if err = scanReferenceOutput(ctx, target, root, p, mode, false); err != nil {
		return r, err
	}
	var failures []error
	for i, n := range p.Notes {
		if err = checkSource(n); err != nil {
			r.Results[i].State = "failed"
			r.Results[i].ErrorCode = "REFERENCE_SOURCE_CHANGED"
			failures = append(failures, err)
			break
		}
		content := []byte(n.After)
		if mode == "rollback" {
			content = []byte(n.Before)
		}
		present, writeErr := writeReferenceNote(ctx, target, root, n.Path, content)
		if writeErr != nil {
			r.Results[i].State = "failed"
			r.Results[i].ErrorCode = "REFERENCE_COPY_INCOMPLETE"
			failures = append(failures, writeErr)
			break
		}
		r.Results[i].State = "copied"
		if present {
			r.Results[i].State = "present"
		}
		r.Results[i].OutputSHA256 = importSHA(content)
	}
	if len(failures) == 0 {
		for _, n := range p.Notes {
			if err = checkSource(n); err != nil {
				failures = append(failures, err)
				break
			}
		}
	}
	if len(failures) == 0 {
		if err = scanReferenceOutput(ctx, target, root, p, mode, true); err != nil {
			failures = append(failures, err)
		}
	}
	r.Complete = len(failures) == 0
	return r, errors.Join(failures...)
}
func referenceBindingsEqual(a, b referenceBinding) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
func preflightReferenceDestination(root *os.Root, wanted referenceBinding) error {
	var got referenceBinding
	if err := readReferenceState(root, referenceStateName, &got); err == nil {
		if !referenceBindingsEqual(got, wanted) {
			return errors.New("client: reference destination belongs to another plan, mode or declared inventory")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, name := range names {
		if name != referenceLockName {
			return errors.New("client: reference destination must be empty or bound to the same plan")
		}
	}
	return nil
}
func readReferenceState(root *os.Root, name string, target any) error {
	st, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Size() > MaxJSONBytes {
		return errors.New("client: invalid reference state file")
	}
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(st, opened) {
		return errors.New("client: reference state changed while opening")
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxJSONBytes+1))
	if err != nil {
		return err
	}
	return importStrictJSON(b, target)
}
func writeReferenceState(root *os.Root, name string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if int64(len(b)) > MaxJSONBytes {
		return errors.New("client: reference state exceeds local document limit")
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
func lockReferenceRoot(root *os.Root) (func(), error) {
	if st, err := root.Lstat(referenceLockName); err == nil && !st.Mode().IsRegular() {
		return nil, errors.New("client: reference output lock must be a regular file")
	}
	f, err := root.OpenFile(referenceLockName, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	st, err := root.Lstat(referenceLockName)
	opened, e := f.Stat()
	if err != nil || e != nil || !st.Mode().IsRegular() || !os.SameFile(st, opened) {
		f.Close()
		return nil, errors.New("client: reference output lock changed")
	}
	if err = lockFile(f); err != nil {
		f.Close()
		return nil, errors.New("client: another process is using this reference copy")
	}
	return func() { unlockFile(f); f.Close() }, nil
}
func writeReferenceNote(ctx context.Context, rootName string, root *os.Root, name string, content []byte) (bool, error) {
	if err := checkWorksetRoot(rootName, root); err != nil {
		return false, err
	}
	if err := checkWorksetTarget(root, name, false); err != nil {
		return false, err
	}
	if st, err := root.Lstat(filepath.FromSlash(name)); err == nil {
		if !st.Mode().IsRegular() {
			return false, errors.New("client: refusing non-regular reference destination")
		}
		b, err := readReferenceNote(ctx, rootName, root, name)
		if err != nil {
			return false, err
		}
		if !bytes.Equal(b, content) {
			return false, errors.New("client: reference copy was edited; use a new destination without overwriting it")
		}
		if err = cleanupPublishedReferencePart(ctx, rootName, root, name, st, content); err != nil {
			return false, err
		}
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := root.MkdirAll(filepath.Dir(filepath.FromSlash(name)), 0700); err != nil {
		return false, err
	}
	part := filepath.FromSlash(name + referencePartSuffix)
	if st, err := root.Lstat(part); err == nil && !st.Mode().IsRegular() {
		return false, errors.New("client: non-regular reference partial file")
	}
	f, err := root.OpenFile(part, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	current, err := root.Lstat(part)
	if err != nil || !os.SameFile(st, current) || !st.Mode().IsRegular() || st.Size() > int64(len(content)) {
		return false, errors.New("client: reference partial file is incompatible")
	}
	partial, err := io.ReadAll(io.LimitReader(f, int64(len(content))+1))
	if err != nil {
		return false, err
	}
	if len(partial) > len(content) || !bytes.Equal(partial, content[:len(partial)]) {
		return false, errors.New("client: reference partial was edited; preserve it for review")
	}
	if _, err = f.Seek(int64(len(partial)), io.SeekStart); err != nil {
		return false, err
	}
	if _, err = io.Copy(f, contextReader{ctx, bytes.NewReader(content[len(partial):])}); err != nil {
		return false, err
	}
	if err = f.Sync(); err != nil {
		return false, err
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	written, err := io.ReadAll(io.LimitReader(f, int64(len(content))+1))
	if err != nil || !bytes.Equal(written, content) {
		return false, errors.New("client: reference copy verification failed")
	}
	if err = f.Close(); err != nil {
		return false, err
	}
	if err = checkWorksetRoot(rootName, root); err != nil {
		return false, err
	}
	if err = checkWorksetTarget(root, name, false); err != nil {
		return false, err
	}
	current, err = root.Lstat(part)
	if err != nil || !os.SameFile(st, current) || !current.Mode().IsRegular() {
		return false, errors.New("client: reference partial changed before publication")
	}
	if err = root.Link(part, filepath.FromSlash(name)); err != nil {
		return false, err
	}
	if err = root.Remove(part); err != nil {
		return false, err
	}
	if err = checkWorksetRoot(rootName, root); err != nil {
		return false, err
	}
	return false, nil
}

// Link publishes the final file before removing the private part. If a process
// stopped in that window, both names can remain. Only fully identical bytes
// are disposable; an edited, shortened or replaced part must be preserved.
func cleanupPublishedReferencePart(ctx context.Context, rootName string, root *os.Root, name string, final os.FileInfo, content []byte) error {
	part := name + referencePartSuffix
	st, err := root.Lstat(filepath.FromSlash(part))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Size() != int64(len(content)) {
		return errors.New("client: published reference has an incompatible partial; preserve it for review")
	}
	b, err := readReferenceNote(ctx, rootName, root, part)
	if err != nil {
		return err
	}
	if !bytes.Equal(b, content) {
		return errors.New("client: published reference partial was edited; preserve it for review")
	}
	if err = checkWorksetRoot(rootName, root); err != nil {
		return err
	}
	current, err := root.Lstat(filepath.FromSlash(part))
	if err != nil {
		return err
	}
	published, err := root.Lstat(filepath.FromSlash(name))
	if err != nil {
		return err
	}
	if !current.Mode().IsRegular() || !os.SameFile(st, current) || current.Size() != st.Size() || !current.ModTime().Equal(st.ModTime()) || !published.Mode().IsRegular() || !os.SameFile(final, published) {
		return errors.New("client: published reference or partial changed before residue cleanup")
	}
	return root.Remove(filepath.FromSlash(part))
}
func scanReferenceOutput(ctx context.Context, rootName string, root *os.Root, p ReferencePlan, mode string, requireAll bool) error {
	if err := checkWorksetRoot(rootName, root); err != nil {
		return err
	}
	expected := map[string]string{}
	for _, n := range p.Notes {
		content := n.After
		if mode == "rollback" {
			content = n.Before
		}
		expected[n.Path] = content
	}
	found := map[string]bool{}
	err := fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("client: reference output contains a symbolic link")
		}
		if d.IsDir() {
			for path := range expected {
				if strings.HasPrefix(path, name+"/") {
					return nil
				}
			}
			return errors.New("client: unexpected directory in reference copy")
		}
		st, err := d.Info()
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			return errors.New("client: non-regular file in reference copy")
		}
		if name == referenceStateName || name == referenceLockName {
			return nil
		}
		content, ok := expected[name]
		if !ok && strings.HasSuffix(name, referencePartSuffix) {
			base := strings.TrimSuffix(name, referencePartSuffix)
			content, ok = expected[base]
			if !ok {
				return errors.New("client: unexpected reference partial")
			}
			if requireAll {
				return errors.New("client: unfinished reference partial")
			}
			f, err := root.Open(filepath.FromSlash(name))
			if err != nil {
				return err
			}
			b, err := io.ReadAll(io.LimitReader(f, int64(len(content))+1))
			f.Close()
			if err != nil {
				return err
			}
			if len(b) > len(content) || !bytes.Equal(b, []byte(content)[:len(b)]) {
				return errors.New("client: edited reference partial")
			}
			return nil
		}
		if !ok {
			return errors.New("client: unexpected file in reference copy")
		}
		b, err := readReferenceNote(ctx, rootName, root, name)
		if err != nil {
			return err
		}
		if !bytes.Equal(b, []byte(content)) {
			return errors.New("client: reference copy contains edits; refusing to overwrite")
		}
		found[name] = true
		return nil
	})
	if err != nil {
		return err
	}
	if requireAll && len(found) != len(expected) {
		return errors.New("client: reference copy is incomplete")
	}
	return checkWorksetRoot(rootName, root)
}
