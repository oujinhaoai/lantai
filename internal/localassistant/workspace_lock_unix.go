//go:build !windows

package localassistant

import (
	"golang.org/x/sys/unix"
	"os"
)

func lockWorkerFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) }
func unlockWorkerFile(f *os.File)     { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }
