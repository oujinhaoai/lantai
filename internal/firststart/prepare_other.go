//go:build !linux && !darwin

package firststart

import (
	"errors"
	"os"
)

func PrepareVolumes(_ []string, _, _ int) error {
	return errors.New("volume preparation requires a Linux container")
}
func privateMetadata(_ os.FileInfo) bool { return false }
