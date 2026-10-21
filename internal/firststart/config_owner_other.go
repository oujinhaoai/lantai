//go:build !linux && !darwin

package firststart

import "os"

func singleConfigLink(_ os.FileInfo) bool   { return false }
func bootstrapPartOwner(_ os.FileInfo) bool { return false }
func legacyEmptyConfig(_ os.FileInfo) bool  { return false }
