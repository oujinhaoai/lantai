//go:build linux || darwin || freebsd

package fsutil

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// FreeBytes 返回 path 所在文件系统中非特权用户可用的字节数。
func FreeBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("fsutil: statfs %s: %w", path, err)
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
