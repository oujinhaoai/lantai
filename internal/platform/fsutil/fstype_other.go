//go:build !(linux || darwin || windows)

package fsutil

// Inspect 在未实现的平台返回“无法判断”。
func Inspect(string) (FSInfo, error) { return FSInfo{Type: "unknown"}, nil }
