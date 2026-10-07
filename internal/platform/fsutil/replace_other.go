//go:build !windows

package fsutil

import "os"

func replaceAtomicFile(source, target string) error { return os.Rename(source, target) }
