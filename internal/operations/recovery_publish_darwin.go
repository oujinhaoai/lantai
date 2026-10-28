package operations

import (
	"os"

	"golang.org/x/sys/unix"
)

func publishRecoveryPackage(temp, target string) error {
	if err := unix.RenamexNp(temp, target, unix.RENAME_EXCL); err != nil {
		return &os.LinkError{Op: "rename-excl", Old: temp, New: target, Err: err}
	}
	return nil
}
