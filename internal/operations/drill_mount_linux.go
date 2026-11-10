//go:build linux

package operations

import (
	"errors"
	"os"
)

func drillMountBoundary(home string) error {
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return errors.New("operations: cannot verify drill mount isolation")
	}
	return validateDrillMountInfo(home, raw)
}
