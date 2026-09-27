//go:build unix

package fileop

import (
	"errors"
	"syscall"
)

func isFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}

func isUnavailable(err error) bool {
	for _, e := range []syscall.Errno{syscall.EBUSY, syscall.ETXTBSY, syscall.EROFS, syscall.EIO, syscall.EACCES, syscall.EPERM, syscall.EAGAIN} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// linkUnsupported 报告硬链接失败是否因为文件系统不支持或跨卷（此时退回复制）。
// Linux 在不支持硬链接的文件系统（如 vfat）上返回 EPERM。
func linkUnsupported(err error) bool {
	for _, e := range []syscall.Errno{syscall.EXDEV, syscall.EPERM, syscall.ENOTSUP, syscall.EOPNOTSUPP, syscall.EMLINK, syscall.ENOSYS} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}
