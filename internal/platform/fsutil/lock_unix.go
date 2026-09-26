//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package fsutil

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// flock 锁归属于打开的文件描述，同一进程再次打开同一文件也会冲突，
// 因此两个实例即使在同一进程内也不能同时持有数据根锁。
func lockFile(f *os.File) error {
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.EWOULDBLOCK):
			return ErrLocked
		default:
			return fmt.Errorf("fsutil: flock: %w", err)
		}
	}
}

func unlockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
