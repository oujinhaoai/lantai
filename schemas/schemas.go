// Package schemas 以只读文件系统嵌入兰台的权威 JSON Schema。
//
// 本目录中的 *.schema.json 与 index.json 是数据、事件、证据和执行协议的
// 唯一定义；Go 代码通过 internal/contract/schema 加载并校验，不另写一份
// 字段定义。本包不含逻辑，只负责嵌入。
package schemas

import "embed"

// BaseURI 是所有 schema $id 的公共前缀，对应仓库 main 分支上的原始文件。
// 校验时一律从嵌入文件解析，不访问网络。
const BaseURI = "https://github.com/oujinhaoai/lantai/raw/main/schemas/"

// FS 包含 index.json 与各 schema 文件（不含 examples）。
//
//go:embed index.json common operations
var FS embed.FS
