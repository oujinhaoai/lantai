//go:build windows

package fileop

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

func isFull(err error) bool {
	return errors.Is(err, windows.ERROR_DISK_FULL) || errors.Is(err, windows.ERROR_HANDLE_DISK_FULL) ||
		errors.Is(err, syscall.ENOSPC)
}

func isUnavailable(err error) bool {
	for _, e := range []error{windows.ERROR_SHARING_VIOLATION, windows.ERROR_LOCK_VIOLATION, windows.ERROR_ACCESS_DENIED,
		windows.ERROR_WRITE_PROTECT, windows.ERROR_NOT_READY, windows.ERROR_USER_MAPPED_FILE, syscall.EBUSY, syscall.EIO, syscall.EROFS} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// linkUnsupported 报告硬链接失败是否因为文件系统不支持（如 FAT/exFAT）或跨卷。
func linkUnsupported(err error) bool {
	for _, e := range []error{windows.ERROR_NOT_SAME_DEVICE, windows.ERROR_INVALID_FUNCTION, windows.ERROR_NOT_SUPPORTED,
		windows.ERROR_TOO_MANY_LINKS, syscall.EXDEV, syscall.ENOTSUP} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}
