package client

import (
	"errors"
	"io"
	"os"
)

// ReadPrivateFromRoot preserves an already pinned workspace boundary while
// applying the same portable owner/ACL checks as private state publication.
func ReadPrivateFromRoot(root *os.Root, name string) ([]byte, error) {
	st, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("client: private input must be regular, not a symbolic link")
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(st, opened) {
		return nil, errors.New("client: private input changed while opening")
	}
	if err := checkPinnedOutputPrivate(f); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxJSONBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > MaxJSONBytes {
		return nil, errors.New("client: private input exceeds byte budget")
	}
	current, err := root.Lstat(name)
	if err != nil || !os.SameFile(opened, current) {
		return nil, errors.New("client: private input replaced while reading")
	}
	return raw, nil
}
