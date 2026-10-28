//go:build linux || darwin

package operations

import (
	"os"
	"syscall"
)

const recoveryExportOpenFlags = syscall.O_NOFOLLOW | syscall.O_NONBLOCK

func recoveryExportMetadata(st os.FileInfo) error {
	if st.Mode().Perm() != 0600 {
		return packageAccessError("permissions")
	}
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok || int(s.Uid) != os.Geteuid() {
		return packageAccessError("owner")
	}
	if s.Nlink != 1 {
		return packageAccessError("hardlinks")
	}
	return nil
}
