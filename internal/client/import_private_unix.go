//go:build !windows

package client

import "os"

func makeImportPrivateDirectory(dir string) error { return os.MkdirAll(dir, 0700) }
