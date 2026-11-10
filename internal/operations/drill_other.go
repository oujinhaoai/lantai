//go:build !linux && !darwin

package operations

import "os"

// Refuse to claim independent files where link identity is not implemented.
func drillIndependentFile(root, entry os.FileInfo) bool { return false }
