//go:build windows

package fsutil

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// Inspect 返回 path 所在卷的类别；以驱动器类型 DRIVE_REMOTE 判断网络卷。
func Inspect(path string) (FSInfo, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return FSInfo{}, err
	}
	p, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return FSInfo{}, err
	}
	vol := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumePathName(p, &vol[0], uint32(len(vol))); err != nil {
		return FSInfo{}, fmt.Errorf("fsutil: GetVolumePathName %s: %w", abs, err)
	}
	info := FSInfo{Known: true}
	fsName := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumeInformation(&vol[0], nil, 0, nil, nil, nil, &fsName[0], uint32(len(fsName))); err == nil {
		info.Type = windows.UTF16ToString(fsName)
	}
	if windows.GetDriveType(&vol[0]) == windows.DRIVE_REMOTE {
		info.Remote = true
	}
	return info, nil
}
