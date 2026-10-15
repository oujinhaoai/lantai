//go:build !linux && !darwin

package identity

import "os"

// Other platforms retain their existing ACL boundary. POSIX ownership checks
// and maintenance reauthentication support are validated on Linux and Darwin.
func controlAuthOwner(os.FileInfo) bool      { return true }
func controlAuthSingleLink(os.FileInfo) bool { return true }

func controlAuthMode(os.FileInfo, bool) bool { return true }
