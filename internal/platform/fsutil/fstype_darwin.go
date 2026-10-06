//go:build darwin

package fsutil

import (
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

// Inspect 返回 path 所在文件系统的类别；macOS 以 MNT_LOCAL 标志判断是否本机，
// 类型名含 fuse 的（macfuse、osxfuse 等）记为 FUSE。
func Inspect(path string) (FSInfo, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return FSInfo{}, fmt.Errorf("fsutil: statfs %s: %w", path, err)
	}
	name := unix.ByteSliceToString(st.Fstypename[:])
	return FSInfo{Type: name, Remote: st.Flags&unix.MNT_LOCAL == 0, Known: true, FUSE: strings.Contains(strings.ToLower(name), "fuse")}, nil
}
