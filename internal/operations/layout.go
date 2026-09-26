// Package operations 负责实例生命周期（T08）：配置、数据根单实例锁、五库连接
// 与按库迁移、实例兼容矩阵、维护屏障、启动恢复与就绪门禁、优雅退出。
//
// 业务表的迁移由所属模块编写（internal/platform/sqlite/migrations），这里只管
// 顺序、兼容性与屏障；每个迁移在所属库内单独成事务，不跨库。实例只允许一个
// 核心进程持有数据根写锁，进程内的维护屏障与锁顺序由 commands.Gate 与
// commands.Coordinator 实现，多进程写同一数据根不在支持范围内。
package operations

import (
	"fmt"
	"path/filepath"

	"github.com/oujinhaoai/lantai/internal/contract/ownership"
)

// Layout 是数据根目录下各文件的位置。数据根必须位于服务端本机磁盘。
type Layout struct {
	// Home 是数据根目录的绝对路径（LANTAI_HOME）。
	Home string
}

// NewLayout 把 home 转为绝对路径。
func NewLayout(home string) (Layout, error) {
	if home == "" {
		return Layout{}, fmt.Errorf("operations: data root directory is not set (use -home or LANTAI_HOME)")
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		return Layout{}, err
	}
	return Layout{Home: filepath.Clean(abs)}, nil
}

// MarkerPath 是实例标记 instance.json。
func (l Layout) MarkerPath() string { return filepath.Join(l.Home, "instance.json") }

// LockPath 是单实例锁文件。
func (l Layout) LockPath() string { return filepath.Join(l.Home, "lantai.lock") }

// ConfigPath 是可选的 config.yaml。
func (l Layout) ConfigPath() string { return filepath.Join(l.Home, "config.yaml") }

// DBDir 是五库所在目录。
func (l Layout) DBDir() string { return filepath.Join(l.Home, "db") }

// DBPath 返回某个库的文件路径。
func (l Layout) DBPath(db ownership.Database) string {
	return filepath.Join(l.DBDir(), string(db)+".db")
}

// authoritative 列出不能自动重建、缺失即进入恢复诊断的库；index 可删除重建。
var authoritative = []ownership.Database{ownership.Main, ownership.Ledger, ownership.Runtime, ownership.Events}

func isAuthoritative(db ownership.Database) bool {
	for _, d := range authoritative {
		if d == db {
			return true
		}
	}
	return false
}
