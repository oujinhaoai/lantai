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

func (o *PinnedPrivateOutput) lockState() (func(), error) {
	if err := o.check(); err != nil {
		return nil, err
	}
	name := o.name + ".lock"
	st, err := o.root.Lstat(name)
	var f *os.File
	if errors.Is(err, os.ErrNotExist) {
		f, err = createPinnedOutputTemp(o.root, name)
		if errors.Is(err, os.ErrExist) {
			f, err = o.root.OpenFile(name, os.O_RDWR, 0)
		}
	} else if err == nil && st.Mode().IsRegular() {
		f, err = o.root.OpenFile(name, os.O_RDWR, 0)
	} else if err == nil {
		err = errors.New("client: state lock must be a regular file")
	}
	if err != nil {
		return nil, err
	}
	st, err = o.root.Lstat(name)
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
