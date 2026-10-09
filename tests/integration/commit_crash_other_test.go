//go:build !windows

package integration

import (
	"errors"
	"os"
)

func crashReadyReadPending(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}
