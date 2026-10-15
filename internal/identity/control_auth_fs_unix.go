//go:build linux || darwin

package identity

import (
	"os"
	"syscall"
)

func controlAuthOwner(st os.FileInfo) bool {
	v, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(v.Uid) == os.Geteuid()
}
func controlAuthSingleLink(st os.FileInfo) bool {
	v, ok := st.Sys().(*syscall.Stat_t)
	return ok && v.Nlink == 1
}

func controlAuthMode(st os.FileInfo, private bool) bool {
	mask := os.FileMode(0022)
	if private {
		mask = 0077
	}
	return st.Mode().Perm()&mask == 0
}
