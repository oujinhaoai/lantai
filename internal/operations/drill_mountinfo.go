package operations

import (
	"errors"
	"path"
	"strings"
)

// validateDrillMountInfo rejects nested mounts, mounted-device aliases, and
// subdirectory mounts. A host subdirectory bind can be the only visible mount
// for its device in a container, so device-count checks alone are insufficient.
// Non-root filesystem subvolumes are deliberately rejected as unproven too.
func validateDrillMountInfo(home string, raw []byte) error {
	type mount struct{ device, root, point string }
	var mounts []mount
	count := map[string]int{}
	rootVisible := false
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		separator := -1
		for idx := 6; idx < len(fields); idx++ {
			if fields[idx] == "-" {
				separator = idx
				break
			}
		}
		if separator < 6 || len(fields) < separator+4 {
			return errors.New("operations: invalid mount isolation evidence")
		}
		root, point := unescape.Replace(fields[3]), unescape.Replace(fields[4])
		if !path.IsAbs(root) || !path.IsAbs(point) || path.Clean(root) != root || path.Clean(point) != point {
			return errors.New("operations: invalid mount isolation paths")
		}
		mounts = append(mounts, mount{fields[2], root, point})
		count[fields[2]]++
		rootVisible = rootVisible || point == "/"
	}
	if len(mounts) == 0 || !rootVisible {
		return errors.New("operations: missing mount isolation evidence")
	}
	for _, m := range mounts {
		if m.point == home || strings.HasPrefix(m.point, home+"/") {
			return errors.New("operations: drill home must not contain mount points")
		}
		ancestor := m.point == "/" || strings.HasPrefix(home, m.point+"/")
		if ancestor && m.root != "/" {
			return errors.New("operations: drill home is on an unproven filesystem subdirectory mount")
		}
		if ancestor && count[m.device] > 1 {
			return errors.New("operations: drill home has a mounted filesystem alias")
		}
	}
	return nil
}
