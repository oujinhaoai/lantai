//go:build !windows

package fileop

import "os"

func holdNativeFile(path string) (*os.File, error) { return os.Open(path) }
