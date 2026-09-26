package fsutil

// FSInfo 是路径所在文件系统的类别。Remote 为 true 表示可以确认是网络文件
// 系统（NFS、SMB 等）；Known 为 false 表示本平台无法判断，调用方按“未验证”
// 处理，不能当作已确认的本机磁盘。
type FSInfo struct {
	Type   string `json:"type"`
	Remote bool   `json:"remote"`
	Known  bool   `json:"known"`
}
