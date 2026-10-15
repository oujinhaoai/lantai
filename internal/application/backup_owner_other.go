//go:build !linux && !darwin

package application

import "os"

func backupOwner(os.FileInfo) bool { return false }
