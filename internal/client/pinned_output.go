package client

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

// PinnedPrivateOutput publishes one bounded private JSON document through a
// pinned parent directory. It never re-resolves the output path for a write.
// Callers must first exclude their source, state and working-copy directories.
type PinnedPrivateOutput struct {
	root     *os.Root
	parent   string
	name     string
	ownsRoot bool
}

// OpenPinnedPrivateOutput expects a physical path without symlink components.
// Missing parent directories are created through the same anchored root walk
// used by working sets, before the caller begins its potentially long action.
func OpenPinnedPrivateOutput(path string) (*PinnedPrivateOutput, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	parent, name := filepath.Dir(abs), filepath.Base(abs)
	if name == "." || name == string(filepath.Separator) {
		return nil, errors.New("client: output file name is required")
	}
	root, err := openWorksetRoot(parent, true)
	if err != nil {
		return nil, err
	}
	o := &PinnedPrivateOutput{root: root, parent: parent, name: name, ownsRoot: true}
	if err = o.check(); err != nil {
		root.Close()
		return nil, err
	}
	return o, nil
}

func (o *PinnedPrivateOutput) Close() error {
	if o == nil || o.root == nil {
		return nil
	}
	r := o.root
	o.root = nil
	if !o.ownsRoot {
		return nil
	}
	return r.Close()
}

func (o *PinnedPrivateOutput) check() error {
	if o == nil || o.root == nil {
		return errors.New("client: private output is closed")
	}
	if err := checkWorksetRoot(o.parent, o.root); err != nil {
		return err
	}
	st, err := o.root.Lstat(o.name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return errors.New("client: refusing non-regular private output")
	}
	f, err := o.root.Open(o.name)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(st, opened) {
		return errors.New("client: private output changed while opening")
	}
	return checkPinnedOutputPrivate(f)
}

func (o *PinnedPrivateOutput) Write(b []byte) error {
	return o.write(b, true)
}

// WriteOriginal publishes an immutable plan. An existing private byte-identical
// file may resume; a different file is never replaced, even by concurrent calls.
func (o *PinnedPrivateOutput) WriteOriginal(b []byte) error {
	return o.write(b, false)
}

// WriteBinaryOriginal is exclusively for explicitly selected bounded artifacts.
// JSON/state writers retain their existing8MiB limit and overwrite behavior.
func (o *PinnedPrivateOutput) WriteBinaryOriginal(b []byte) error {
	return o.writeBounded(b, false, 65<<20)
}
func (o *PinnedPrivateOutput) write(b []byte, replace bool) error {
	return o.writeBounded(b, replace, MaxJSONBytes)
}
func (o *PinnedPrivateOutput) writeBounded(b []byte, replace bool, limit int64) error {
	if int64(len(b)) > limit {
		return errors.New("client: local state exceeds size limit")
	}
	if err := o.check(); err != nil {
		return err
	}
	var f *os.File
	var name string
	var err error
	for range 10 {
		name = ".lantai-output-" + rand.Text()
		f, err = createPinnedOutputTemp(o.root, name)
		if !errors.Is(err, os.ErrExist) {
			break
		}
	}
	if err != nil {
		return err
	}
	defer func() {
		// Windows does not permit deletion while the private write handle is open.
		_ = f.Close()
		_ = o.root.Remove(name)
	}()
	if n, err := f.Write(b); err != nil {
		return err
	} else if n != len(b) {
		return io.ErrShortWrite
	}
	if err = f.Sync(); err != nil {
		return err
	}
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	current, err := o.root.Lstat(name)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		return errors.New("client: private output temporary file changed")
	}
	if err = checkPinnedOutputPrivate(f); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = o.check(); err != nil {
		return err
	}
	if replace {
		if err = o.root.Rename(name, o.name); err != nil {
			return err
		}
	} else {
		if err = o.root.Link(name, o.name); errors.Is(err, os.ErrExist) {
			return o.checkOriginal(b)
		} else if err != nil {
			return err
		}
	}
	current, err = o.root.Lstat(o.name)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		return errors.New("client: private output changed while publishing")
	}
	if err = syncPinnedOutputDirectory(o.root); err != nil {
		return err
	}
	return checkWorksetRoot(o.parent, o.root)
}

func (o *PinnedPrivateOutput) checkOriginal(want []byte) error {
	if err := o.check(); err != nil {
		return err
	}
	st, err := o.root.Lstat(o.name)
	if err != nil {
		return err
	}
	f, err := o.root.Open(o.name)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(st, opened) || !opened.Mode().IsRegular() {
		return errcode.New(errcode.PathConflict, "original plan changed while opening")
	}
	if err := checkPinnedOutputPrivate(f); err != nil {
		return err
	}
	got, err := io.ReadAll(io.LimitReader(f, int64(len(want))+1))
	if err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return errcode.New(errcode.PathConflict, "existing original plan differs; preserve original input and state")
	}
	current, err := o.root.Lstat(o.name)
	if err != nil || !os.SameFile(opened, current) {
		return errcode.New(errcode.PathConflict, "original plan replaced during comparison")
	}
	if err := syncPinnedOutputDirectory(o.root); err != nil {
		return err
	}
	return checkWorksetRoot(o.parent, o.root)
}

// OpenBatchPrivateOutput reuses the existing canonical path identity and pinned
// publication, excluding every original source/state/working-copy root first.
func OpenBatchPrivateOutput(path string, protectedRoots ...string) (*PinnedPrivateOutput, error) {
	physical, err := importAbsolute(path)
	if err != nil {
		return nil, err
	}
	for _, root := range protectedRoots {
		if root == "" {
			continue
		}
		canonical, err := importAbsolute(root)
		if err != nil {
			return nil, err
		}
		if err := importDisjoint(physical, canonical); err != nil {
			return nil, errcode.New(errcode.PathConflict, "batch output overlaps original source, state or working copy")
		}
	}
	return OpenPinnedPrivateOutput(physical)
}
