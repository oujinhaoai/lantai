//go:build !linux && !darwin

package operations

import "os"

// Keep the existing exclusive publication on other platforms. Native
// no-replace rename and hard-link metadata need separate platform validation.
func publishRecoveryPackage(temp, target string) error {
	if err := os.Link(temp, target); err != nil {
		return err
	}
	return os.Remove(temp)
}
