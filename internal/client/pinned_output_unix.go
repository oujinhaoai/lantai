//go:build !windows

package client

import "os"

func createPinnedOutputTemp(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
}
func checkPinnedOutputPrivate(f *os.File) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	return checkPrivate("", st)
}
func syncPinnedOutputDirectory(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
