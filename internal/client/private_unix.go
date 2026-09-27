//go:build !windows

package client

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func checkPrivate(_ string, st os.FileInfo) error {
	if st.Mode().Perm()&0077 != 0 {
		return errors.New("client: credential/state file permissions must be 0600")
	}
	return nil
}
func lockFile(f *os.File) error                { return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) }
func unlockFile(f *os.File)                    { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }
func privateTemp(dir string) (*os.File, error) { return os.CreateTemp(dir, ".lantai-state-*") }
func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
