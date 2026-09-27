// Package plugins 嵌入随核心编译发布的官方包文件。不存在磁盘扫描或动态 loader。
package plugins

import "embed"

// Files 是只读发布内容。登记入口只枚举源码中固定的内置清单。
//
//go:embed corecheck/extension.yaml corecheck/config.schema.json corecheck/LICENSE corecheck/check.go
var Files embed.FS
