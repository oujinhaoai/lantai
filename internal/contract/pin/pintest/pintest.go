// Package pintest 提供 pin.Registry 的内存桩。
package pintest

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/pin"
)

// Memory 是内存 pin 注册表。
type Memory struct {
	mu   sync.Mutex
	pins map[ids.ID]pin.Pin
	// Fail 非空时 PinsFor 返回该错误，模拟来源不可用。
	Fail error
}

var _ pin.Registry = (*Memory)(nil)

// New 创建注册表。
func New() *Memory { return &Memory{pins: map[ids.ID]pin.Pin{}} }

// Add 实现 pin.Registry：同 ID 同内容幂等，异内容拒绝。
func (m *Memory) Add(_ context.Context, p pin.Pin) error {
	if err := p.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.pins[p.PinID]; ok {
		if old.Kind != p.Kind || old.Owner != p.Owner || !slices.Equal(old.Blobs, p.Blobs) {
			return errcode.New(errcode.IdempotencyConflict, "pin id reused for different content")
		}
		return nil
	}
	m.pins[p.PinID] = p
	return nil
}

// Release 实现 pin.Registry。
func (m *Memory) Release(_ context.Context, id ids.ID, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pins[id]
	if !ok {
		return errcode.New(errcode.NotFound, "")
	}
	if p.ReleasedAt.IsZero() {
		p.ReleasedAt = at
		m.pins[id] = p
	}
	return nil
}

// PinsFor 实现 pin.Source。
func (m *Memory) PinsFor(_ context.Context, sha256 string) ([]pin.Pin, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	var out []pin.Pin
	for _, p := range m.pins {
		if slices.Contains(p.Blobs, sha256) {
			out = append(out, p)
		}
	}
	return out, nil
}

// ErrUnavailable 可用于 Fail 字段。
var ErrUnavailable = errors.New("pintest: source unavailable")
