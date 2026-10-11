//go:build linux || darwin

package firststart

import (
	"os"
	"syscall"
)

func privateOwner(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid() && (info.IsDir() || st.Nlink == 1)
}
