// Package clock 定义可替换的时间源契约。
//
// 租约、到期、回收站保留期等判定都以服务端时间为准；领域代码只通过 Clock
// 取时间，测试用 Fake 精确推进，不依赖 time.Sleep。持久化统一使用 UTC
// 毫秒，见 Millis/FromMillis。
package clock

import (
	"fmt"
	"regexp"
	"sync"
	"time"
)

// Clock 是领域代码唯一的时间来源。
type Clock interface {
	Now() time.Time
}

// System 返回截断到毫秒的 UTC 系统时间。
type System struct{}

// Now 实现 Clock。
func (System) Now() time.Time { return Truncate(time.Now()) }

// Truncate 把时间转换为 UTC 并截断到毫秒，与持久化精度一致。
func Truncate(t time.Time) time.Time { return t.UTC().Truncate(time.Millisecond) }

// Millis 返回 Unix 毫秒，用于 SQLite 整数列。
func Millis(t time.Time) int64 { return t.UnixMilli() }

// FromMillis 把 Unix 毫秒还原为 UTC 时间。
func FromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// Layout 是契约时间戳格式：UTC、毫秒精度、以 Z 结尾。
const Layout = "2006-01-02T15:04:05.000Z07:00"

var timestampRE = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3}Z$`)

// Format 按契约格式输出时间，例如 2026-09-26T10:14:34.123Z。
func Format(t time.Time) string { return Truncate(t).Format(Layout) }

// Parse 严格解析契约格式的时间戳；带时区偏移或缺少毫秒一律拒绝。
func Parse(s string) (time.Time, error) {
	if !timestampRE.MatchString(s) {
		return time.Time{}, fmt.Errorf("clock: %q is not a UTC millisecond timestamp", s)
	}
	t, err := time.Parse(Layout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("clock: %w", err)
	}
	return t.UTC(), nil
}

// Fake 是测试用的可控时钟，并发安全。
type Fake struct {
	mu  sync.Mutex
	now time.Time
}

// NewFake 以给定时间（截断到毫秒）创建可控时钟。
func NewFake(start time.Time) *Fake { return &Fake{now: Truncate(start)} }

// Now 实现 Clock。
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance 把时钟向前推进 d，d 为负时 panic，避免测试里出现时间倒流。
func (f *Fake) Advance(d time.Duration) time.Time {
	if d < 0 {
		panic("clock: Fake.Advance with negative duration")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = Truncate(f.now.Add(d))
	return f.now
}

// Set 把时钟设为 t；只允许不早于当前时间。
func (f *Fake) Set(t time.Time) {
	t = Truncate(t)
	f.mu.Lock()
	defer f.mu.Unlock()
	if t.Before(f.now) {
		panic("clock: Fake.Set would move time backwards")
	}
	f.now = t
}
