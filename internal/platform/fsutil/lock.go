// Package fsutil 提供实例运维需要的本机文件系统能力：数据根目录的独占锁、
// 原子替换写入、磁盘余量与文件系统类别探测。
//
// 这些能力按平台实现，只覆盖 Linux、macOS 与 Windows（以及同样提供 flock
// 的 BSD）；其他平台明确返回 ErrUnsupported，不静默降级。交叉编译通过不代表
// 平台行为已经验证，实测结果记录在各平台的验收记录中。
package fsutil

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// ErrLocked 表示锁已被另一个进程（或同一进程的另一个句柄）持有。
var ErrLocked = errors.New("fsutil: lock is held by another process")

// ErrUnsupported 表示当前平台没有实现该能力。
var ErrUnsupported = errors.New("fsutil: not supported on this platform")

// Lock 是一个文件上的独占锁。锁由操作系统维护：进程退出或崩溃后自动释放，
// 不会留下需要人工清理的陈旧锁。锁只在本机有效，数据根目录必须位于本机磁盘。
type Lock struct {
	f    *os.File
	path string
}

// Holder 是写入锁文件的诊断信息，只用于报告，不参与判定。
type Holder struct {
	PID        int    `json:"pid"`
	AcquiredAt string `json:"acquired_at"`
}

// TryLock 以非阻塞方式取得 path 上的独占锁；文件不存在时创建。已被持有时
// 返回 ErrLocked。取得后把进程号与时间写入文件，便于诊断。
func TryLock(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("fsutil: open lock file: %w", err)
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, err
	}
	l := &Lock{f: f, path: path}
	info, _ := json.Marshal(Holder{PID: os.Getpid(), AcquiredAt: time.Now().UTC().Format(time.RFC3339)})
	// 诊断信息写入失败不影响锁本身。
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt(append(info, '\n'), 0)
	}
	return l, nil
}

// Path 返回锁文件路径。
func (l *Lock) Path() string { return l.path }

// Unlock 释放锁并关闭文件；重复调用返回 nil。锁文件本身保留，下一次取锁复用。
func (l *Lock) Unlock() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := unlockFile(l.f)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}

// ReadHolder 读取锁文件中的诊断信息；不判断锁当前是否被持有。
func ReadHolder(path string) (Holder, error) {
	var h Holder
	raw, err := os.ReadFile(path)
	if err != nil {
		return h, err
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return h, fmt.Errorf("fsutil: lock file %s: %w", path, err)
	}
	return h, nil
}
