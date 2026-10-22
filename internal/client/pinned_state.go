package client

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// Sibling state files retain the owner's pinned directory for an entire batch;
// closing one sibling never closes the batch's directory handle.
func (o *PinnedPrivateOutput) sibling(name string) (*PinnedPrivateOutput, error) {
	if name == "." || name == ".." || filepath.Base(name) != name || name == "" {
		return nil, errors.New("client: state sibling must be one file name")
	}
	if err := o.check(); err != nil {
		return nil, err
	}
	s := &PinnedPrivateOutput{root: o.root, parent: o.parent, name: name}
	if err := s.check(); err != nil {
		return nil, err
	}
	return s, nil
}

func (o *PinnedPrivateOutput) loadState(value any) error {
	if err := o.check(); err != nil {
		return err
	}
	st, err := o.root.Lstat(o.name)
	if err != nil {
		return err
	}
	if st.Size() > MaxJSONBytes {
		return errors.New("client: local state exceeds size limit")
	}
	f, err := o.root.Open(o.name)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(st, opened) {
		return errors.New("client: pinned state changed while opening")
	}
	if err = checkPinnedOutputPrivate(f); err != nil {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxJSONBytes+1))
	if err != nil {
		return err
	}
	if int64(len(b)) > MaxJSONBytes {
		return errors.New("client: local state exceeds size limit")
	}
	current, err := o.root.Lstat(o.name)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(st, current) {
		return errors.New("client: pinned state changed while reading")
	}
	if err = checkWorksetRoot(o.parent, o.root); err != nil {
		return err
	}
	return json.Unmarshal(b, value)
}

func (o *PinnedPrivateOutput) saveState(value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return o.Write(append(b, '\n'))
}

// OpenPrivateLockFile opens or exclusively creates a private regular lock file
// under an already pinned directory. It does not replace an existing inode or
// acquire the caller's platform-specific lock.
func OpenPrivateLockFile(root *os.Root, name string) (*os.File, error) {
	if root == nil || name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return nil, errors.New("client: lock must be one pinned file name")
	}
	f, err := createPinnedOutputTemp(root, name)
	if errors.Is(err, os.ErrExist) {
		f, err = root.OpenFile(name, os.O_RDWR, 0)
	}
	if err != nil {
		return nil, err
	}
	current, statErr := root.Lstat(name)
	opened, openErr := f.Stat()
	if statErr != nil || openErr != nil || !current.Mode().IsRegular() || !os.SameFile(current, opened) {
		f.Close()
		return nil, errors.New("client: private lock changed while opening")
	}
	if err = checkPinnedOutputPrivate(f); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func (o *PinnedPrivateOutput) lockState() (func(), error) {
	if err := o.check(); err != nil {
		return nil, err
	}
	name := o.name + ".lock"
	f, err := OpenPrivateLockFile(o.root, name)
	if err != nil {
		return nil, err
	}
	st, err := o.root.Lstat(name)
	opened, openErr := f.Stat()
	if err != nil || openErr != nil || !st.Mode().IsRegular() || !os.SameFile(st, opened) {
		f.Close()
		return nil, errors.New("client: pinned state lock changed while opening")
	}
	if err = lockFile(f); err != nil {
		f.Close()
		return nil, errors.New("client: another process is using this transfer state")
	}
	if err = checkWorksetRoot(o.parent, o.root); err != nil {
		unlockFile(f)
		f.Close()
		return nil, err
	}
	return func() { unlockFile(f); f.Close() }, nil
}
