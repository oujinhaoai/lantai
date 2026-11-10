package operations

import "testing"

func TestDrillMountInfo(t *testing.T) {
	const root = "1 0 0:1 / / rw - overlay overlay rw\n"
	for _, tc := range []struct {
		name, home, mounts string
		allowed            bool
	}{
		{"filesystem-root", "/home/drill", "1 0 8:1 / / rw - ext4 /dev/sda rw\n", true},
		{"tmpfs", "/scratch/drill", root + "2 1 0:2 / /scratch rw - tmpfs tmpfs rw\n", true},
		{"unique-subdirectory-bind", "/mnt/drill", root + "2 1 8:1 /host/recovery /mnt rw - ext4 /dev/sda rw\n", false},
		{"container-root-subdirectory-bind", "/home/drill", "1 0 8:1 /host/container / rw - ext4 /dev/sda rw\n", false},
		{"nested-mount", "/home/drill", root + "2 1 0:2 / /home/drill/data rw - tmpfs tmpfs rw\n", false},
		{"exact-mount", "/home/drill", root + "2 1 0:2 / /home/drill rw - tmpfs tmpfs rw\n", false},
		{"mounted-device-alias", "/home/drill", root + "2 1 0:1 / /mirror rw - overlay overlay rw\n", false},
		{"escaped-path", "/scratch area/drill", root + `2 1 0:2 / /scratch\040area rw - tmpfs tmpfs rw` + "\n", true},
		{"escaped-subdirectory-bind", "/scratch area/drill", root + `2 1 8:1 /host\040recovery /scratch\040area rw - ext4 /dev/sda rw` + "\n", false},
		{"escaped-nested-path", "/home/drill", root + `2 1 0:2 / /home/drill/data\040area rw - tmpfs tmpfs rw` + "\n", false},
		{"component-boundary", "/home/drill", root + "2 1 0:2 /subdir /home/drill-other rw - tmpfs tmpfs rw\n", true},
		{"missing-root-proof", "/scratch/drill", "2 1 0:2 / /scratch rw - tmpfs tmpfs rw\n", false},
		{"noncanonical-root", "/home/drill", "1 0 8:1 /host/.. / rw - ext4 /dev/sda rw\n", false},
		{"noncanonical-point", "/home/drill", "1 0 8:1 / /home/.. rw - ext4 /dev/sda rw\n", false},
		{"relative-point", "/home/drill", "1 0 8:1 / relative rw - ext4 /dev/sda rw\n", false},
		{"empty", "/home/drill", "", false},
		{"truncated", "/home/drill", "1 0 8:1 / / rw\n", false},
		{"relative-root", "/home/drill", "1 0 8:1 relative / rw - ext4 /dev/sda rw\n", false},
		{"optional-fields", "/home/drill", "1 0 8:1 / / rw shared:1 - ext4 /dev/sda rw\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDrillMountInfo(tc.home, []byte(tc.mounts))
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v, got %v", tc.allowed, err)
			}
		})
	}
}
