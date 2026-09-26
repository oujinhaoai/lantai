//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || windows)

package fsutil

import "os"

func lockFile(*os.File) error   { return ErrUnsupported }
func unlockFile(*os.File) error { return ErrUnsupported }
