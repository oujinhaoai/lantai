//go:build !linux

package operations

// Darwin aliases are rejected by path/inode checks and nested device identity.
// Unsupported platforms are already rejected by drillIndependentFile.
func drillMountBoundary(home string) error { return nil }
