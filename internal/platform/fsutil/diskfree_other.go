//go:build !(linux || darwin || freebsd || windows)

package fsutil

// FreeBytes 在未实现的平台返回 ErrUnsupported。
func FreeBytes(string) (uint64, error) { return 0, ErrUnsupported }
