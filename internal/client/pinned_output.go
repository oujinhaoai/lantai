package client

import (
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
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
	if int64(len(b)) > MaxJSONBytes {
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
	defer f.Close()
	defer o.root.Remove(name)
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
	if err = o.root.Rename(name, o.name); err != nil {
		return err
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
