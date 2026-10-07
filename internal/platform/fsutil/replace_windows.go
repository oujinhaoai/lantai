//go:build windows

package fsutil

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
	"time"
)

// Retry only the final replacement of the same closed, synced temporary file.
// Never remove the old target or turn a persistent denial into success.
func replaceAtomicFile(source, target string) error {
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := os.Rename(source, target)
		remaining := time.Until(deadline)
		if err == nil || !atomicReplacementRetryable(err) || remaining <= 0 {
			return err
		}
		time.Sleep(min(25*time.Millisecond, remaining))
	}
}

func atomicReplacementRetryable(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
}
