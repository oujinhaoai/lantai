//go:build !windows

package fsutil

import (
	"fmt"
	"os"
)

// CreatePrivateTemp creates a private temporary file; prefix is a simple name.
func CreatePrivateTemp(dir, prefix string) (*os.File, error) {
	return os.CreateTemp(dir, prefix+"*")
}

// CheckPrivateFile verifies a regular private file using the platform's access model.
func CheckPrivateFile(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("%w: private regular file required", ErrInsecurePermissions)
	}
	return nil
}
