package operations

import (
	"os"

	"golang.org/x/sys/unix"
)

// Do not create a second hard link even transiently. Unsupported filesystems
// fail closed instead of falling back to an overwriting rename or a hard link.
func publishRecoveryPackage(temp, target string) error {
	if err := unix.Renameat2(unix.AT_FDCWD, temp, unix.AT_FDCWD, target, unix.RENAME_NOREPLACE); err != nil {
		return &os.LinkError{Op: "rename-noreplace", Old: temp, New: target, Err: err}
	}
	return nil
}
