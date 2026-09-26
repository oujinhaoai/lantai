//go:build linux

package fsutil

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Linux statfs f_type 魔数，取自 linux/magic.h 与各文件系统源码。
var linuxFS = map[uint32]struct {
	name   string
	remote bool
}{
	0xEF53:     {"ext4", false},
	0x58465342: {"xfs", false},
	0x9123683E: {"btrfs", false},
	0x01021994: {"tmpfs", false},
	0x794C7630: {"overlayfs", false},
	0x2FC12FC1: {"zfs", false},
	0xF2F52010: {"f2fs", false},
	0x5346544E: {"ntfs", false},
	0x4D44:     {"vfat", false},
	0x2011BAB0: {"exfat", false},
	0x65735546: {"fuse", false}, // 可能是本机或远程（sshfs 等），魔数区分不了：报告为无法判断（Known 为 false）
	0x6969:     {"nfs", true},
	0x517B:     {"smb", true},
	0xFF534D42: {"cifs", true},
	0xFE534D42: {"smb2", true},
	0x73757245: {"coda", true},
	0x5346414F: {"afs", true},
	0x00C36400: {"ceph", true},
	0x0BD00BD0: {"lustre", true},
	0x01021997: {"9p", true},
	0x6B414653: {"kafs", true},
}

// Inspect 返回 path 所在文件系统的类别。
func Inspect(path string) (FSInfo, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return FSInfo{}, fmt.Errorf("fsutil: statfs %s: %w", path, err)
	}
	magic := uint32(st.Type)
	if fs, ok := linuxFS[magic]; ok {
		return FSInfo{Type: fs.name, Remote: fs.remote, Known: fs.name != "fuse"}, nil
	}
	return FSInfo{Type: fmt.Sprintf("0x%X", magic)}, nil
}
