//go:build windows

package fsutil

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// FreeBytes 返回 path 所在卷中调用者可用的字节数。
func FreeBytes(path string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return 0, fmt.Errorf("fsutil: GetDiskFreeSpaceEx %s: %w", path, err)
	}
	return free, nil
}
