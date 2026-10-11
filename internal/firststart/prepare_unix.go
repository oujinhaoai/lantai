//go:build linux || darwin

// Package firststart implements the container's protected first-start flow.
package firststart

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// PrepareVolumes changes only the root of a new empty directory. Existing
// contents are never traversed, chowned, removed or initialized by this helper.
// Every directory is validated before any mutation, through no-follow handles.
func PrepareVolumes(paths []string, uid, gid int) error {
	type dir struct {
		f     *os.File
		empty bool
	}
	var dirs []dir
	defer func() {
		for _, d := range dirs {
			_ = d.f.Close()
		}
	}()
	for index, path := range paths {
		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("prepare directory %s: %w", path, err)
		}
		f := os.NewFile(uintptr(fd), path)
		dirs = append(dirs, dir{f: f})
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return err
		}
		entries, err := f.ReadDir(-1)
		if err != nil {
			return err
		}
		dirs[len(dirs)-1].empty = len(entries) == 0
		private := st.Mode&0o777 == 0o700
		// Existing data roots created by the native/old Compose workflow may
		// be 0755. Preserve them if only the owner can write; secrets/setup
		// remain private. Every new empty directory still becomes 0700.
		if index == 0 {
			private = st.Mode&0o700 == 0o700 && st.Mode&0o022 == 0
		}
		if len(entries) > 0 && (int(st.Uid) != uid || int(st.Gid) != gid || !private) {
			return fmt.Errorf("prepare directory %s: existing owner or permissions are unsafe; refusing to change it (expected service %d:%d)", path, uid, gid)
		}
	}
	for _, d := range dirs {
		if !d.empty {
			continue
		}
		if err := d.f.Chown(uid, gid); err != nil {
			return err
		}
		if err := d.f.Chmod(0o700); err != nil {
			return err
		}
	}
	return nil
}

func privateMetadata(info os.FileInfo) bool {
	return privateOwner(info)
}
