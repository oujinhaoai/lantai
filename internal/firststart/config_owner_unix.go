//go:build linux || darwin

package firststart

import (
	"os"
	"syscall"
)

func singleConfigLink(st os.FileInfo) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && s.Nlink == 1
}
func bootstrapPartOwner(st os.FileInfo) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(s.Uid) == os.Geteuid() && (s.Nlink == 1 || s.Nlink == 2)
}
func legacyEmptyConfig(st os.FileInfo) bool {
	if st == nil || !st.Mode().IsRegular() || st.Size() != 0 || st.Mode().Perm()&0o022 != 0 {
		return false
	}
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && s.Uid == 0 && s.Gid == 0 && s.Nlink == 1
}
