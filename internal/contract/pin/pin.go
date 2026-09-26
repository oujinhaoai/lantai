// Package pin 定义阻止 Blob 被 GC 回收的保留记录（lantai.pin/v1）与查询接口。
//
// upload pin 随上传会话（storage，可到期），commit pin 随 prepared 操作
// （ledger），backup pin 随备份复制（operations）；后两类只能由所属操作
// 显式释放，不按时间自动失效。GC 在按哈希的协调锁内查询全部来源；任何
// 来源出错都视为“仍被保留”，GC 暂停而不是删除。
package pin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
)

// Contract 是 pin 记录的契约标识。
const Contract = "lantai.pin/v1"

// Kind 是 pin 类别。
type Kind string

const (
	KindUpload Kind = "upload"
	KindCommit Kind = "commit"
	KindBackup Kind = "backup"
)

// Owner 是持有 pin 的对象。
type Owner struct {
	Kind string `json:"kind"` // upload_session | operation | backup
	ID   ids.ID `json:"id"`
}

// Pin 是一条保留记录。
type Pin struct {
	PinID       ids.ID
	Kind        Kind
	OwnerModule string
	Owner       Owner
	// Blobs 为 sha256 十六进制，升序且不重复。
	Blobs      []string
	CreatedAt  time.Time
	ExpiresAt  time.Time // 仅 upload 可设置
	ReleasedAt time.Time
}

type wire struct {
	PinID       ids.ID   `json:"pin_id"`
	Kind        Kind     `json:"kind"`
	OwnerModule string   `json:"owner_module"`
	Owner       Owner    `json:"owner_ref"`
	Blobs       []string `json:"blobs"`
	CreatedAt   string   `json:"created_at"`
	ExpiresAt   string   `json:"expires_at,omitempty"`
	ReleasedAt  string   `json:"released_at,omitempty"`
}

func formatOpt(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return clock.Format(t)
}

// MarshalJSON 输出契约格式。
func (p Pin) MarshalJSON() ([]byte, error) {
	return json.Marshal(wire{
		PinID: p.PinID, Kind: p.Kind, OwnerModule: p.OwnerModule, Owner: p.Owner, Blobs: p.Blobs,
		CreatedAt: clock.Format(p.CreatedAt), ExpiresAt: formatOpt(p.ExpiresAt), ReleasedAt: formatOpt(p.ReleasedAt),
	})
}

// Validate 按权威 schema 校验，并检查哈希升序。
func (p Pin) Validate() error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	doc, err := canonjson.Decode(raw)
	if err != nil {
		return err
	}
	reg, err := schema.Default()
	if err != nil {
		return err
	}
	if err := reg.Validate(Contract, doc); err != nil {
		return err
	}
	if !slices.IsSorted(p.Blobs) {
		return errors.New("pin: blobs must be sorted")
	}
	return nil
}

// ActiveAt 报告 pin 在 now 是否仍保留内容。
func (p Pin) ActiveAt(now time.Time) bool {
	if !p.ReleasedAt.IsZero() {
		return false
	}
	return p.ExpiresAt.IsZero() || now.Before(p.ExpiresAt)
}

// Source 由持有 pin 的模块实现，返回覆盖某个哈希的记录（含已释放的，由调用方判断）。
type Source interface {
	PinsFor(ctx context.Context, sha256 string) ([]Pin, error)
}

// Registry 管理本模块 pin 的生命周期。Add 以 PinID 幂等；Release 只能由所属操作调用。
type Registry interface {
	Source
	Add(ctx context.Context, p Pin) error
	Release(ctx context.Context, pinID ids.ID, at time.Time) error
}

// Held 报告哈希是否仍被任何来源保留。任一来源出错时返回 true 与错误：
// GC 必须把不确定当作保留并暂停，不能把查询失败当作“无引用”。
func Held(ctx context.Context, sha256 string, now time.Time, sources ...Source) (bool, error) {
	for _, s := range sources {
		pins, err := s.PinsFor(ctx, sha256)
		if err != nil {
			return true, fmt.Errorf("pin: source unavailable, treat %s as held: %w", sha256, err)
		}
		for _, p := range pins {
			if p.ActiveAt(now) && slices.Contains(p.Blobs, sha256) {
				return true, nil
			}
		}
	}
	return false, nil
}
