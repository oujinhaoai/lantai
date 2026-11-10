//go:build linux || darwin

package operations

import (
	"os"
	"syscall"
)

func drillIndependentFile(root, entry os.FileInfo) bool {
	r, ok := root.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	e, ok := entry.Sys().(*syscall.Stat_t)
	return ok && r.Dev == e.Dev && (entry.IsDir() || e.Nlink == 1)
}
