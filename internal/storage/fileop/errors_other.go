//go:build !unix && !windows

package fileop

import (
	"errors"
	"io/fs"
)

func isFull(error) bool { return false }

func isUnavailable(err error) bool { return errors.Is(err, fs.ErrPermission) }

func linkUnsupported(err error) bool { return errors.Is(err, errors.ErrUnsupported) }
