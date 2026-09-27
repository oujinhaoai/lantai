package operations

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
)

// MarkerContract 是实例标记 instance.json 的契约标识。
const MarkerContract = "lantai.instance/v1"

// MarkerState 是实例标记的状态。
type MarkerState string

const (
	// MarkerInitializing 表示初始化尚未完成，只能由 lantai init 继续。
	MarkerInitializing MarkerState = "initializing"
	// MarkerActive 表示初始化已完成。
	MarkerActive MarkerState = "active"
)

// Marker 是实例标记。
type Marker struct {
	InstanceID        ids.ID
	Name              string
	State             MarkerState
	DataFormatVersion int
	RecoveryEpoch     int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
	// Migration 非空表示有一次迁移尚未完成。
	Migration *MigrationRun
	// Restore keeps every entry point closed until credential rotation, domain
	// checks, index reconstruction and a bound local reconciliation review finish.
	Restore *RestoreRun
}

// MigrationRun 是一次迁移的实例级记录。
type MigrationRun struct {
	RunID               ids.ID
	StartedAt           time.Time
	FromFormatVersion   int
	TargetFormatVersion int
	BackupID            ids.ID
	BackupDigest        digest.Digest
}

type markerWire struct {
	Contract          string         `json:"contract"`
	InstanceID        ids.ID         `json:"instance_id"`
	Name              string         `json:"name"`
	State             MarkerState    `json:"state"`
	DataFormatVersion int            `json:"data_format_version"`
	RecoveryEpoch     int64          `json:"recovery_epoch"`
	CreatedAt         string         `json:"created_at"`
	UpdatedAt         string         `json:"updated_at"`
	Migration         *migrationWire `json:"migration,omitempty"`
	Restore           *RestoreRun    `json:"restore,omitempty"`
}

type migrationWire struct {
	RunID               ids.ID        `json:"run_id"`
	StartedAt           string        `json:"started_at"`
	FromFormatVersion   int           `json:"from_format_version"`
	TargetFormatVersion int           `json:"target_format_version"`
	BackupID            ids.ID        `json:"backup_id,omitempty"`
	BackupDigest        digest.Digest `json:"backup_digest,omitempty"`
}

// MarshalJSON 输出契约格式。
func (m Marker) MarshalJSON() ([]byte, error) {
	w := markerWire{
		Contract: MarkerContract, InstanceID: m.InstanceID, Name: m.Name, State: m.State,
		DataFormatVersion: m.DataFormatVersion, RecoveryEpoch: m.RecoveryEpoch,
		CreatedAt: clock.Format(m.CreatedAt), UpdatedAt: clock.Format(m.UpdatedAt),
		Restore: m.Restore,
	}
	if r := m.Migration; r != nil {
		w.Migration = &migrationWire{RunID: r.RunID, StartedAt: clock.Format(r.StartedAt),
			FromFormatVersion: r.FromFormatVersion, TargetFormatVersion: r.TargetFormatVersion, BackupID: r.BackupID, BackupDigest: r.BackupDigest}
	}
	return json.Marshal(w)
}

// ParseMarker 严格解析并按 schema 校验实例标记。
func ParseMarker(raw []byte) (Marker, error) {
	doc, err := canonjson.Decode(raw)
	if err != nil {
		return Marker{}, fmt.Errorf("operations: instance marker: %w", err)
	}
	reg, err := schema.Default()
	if err != nil {
		return Marker{}, err
	}
	if err := reg.Validate(MarkerContract, doc); err != nil {
		return Marker{}, fmt.Errorf("operations: instance marker: %w", err)
	}
	var w markerWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return Marker{}, fmt.Errorf("operations: instance marker: %w", err)
	}
	m := Marker{InstanceID: w.InstanceID, Name: w.Name, State: w.State,
		DataFormatVersion: w.DataFormatVersion, RecoveryEpoch: w.RecoveryEpoch, Restore: w.Restore}
	if m.CreatedAt, err = clock.Parse(w.CreatedAt); err != nil {
		return Marker{}, err
	}
	if m.UpdatedAt, err = clock.Parse(w.UpdatedAt); err != nil {
		return Marker{}, err
	}
	if r := w.Migration; r != nil {
		run := MigrationRun{RunID: r.RunID, FromFormatVersion: r.FromFormatVersion, TargetFormatVersion: r.TargetFormatVersion, BackupID: r.BackupID, BackupDigest: r.BackupDigest}
		if run.StartedAt, err = clock.Parse(r.StartedAt); err != nil {
			return Marker{}, err
		}
		m.Migration = &run
	}
	return m, nil
}

// ReadMarker 读取实例标记；不存在时返回的错误满足 errors.Is(err, fs.ErrNotExist)。
func ReadMarker(l Layout) (Marker, error) {
	raw, err := os.ReadFile(l.MarkerPath())
	if err != nil {
		return Marker{}, err
	}
	return ParseMarker(raw)
}

// writeMarker 按契约校验后原子替换实例标记。
func writeMarker(l Layout, m Marker) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if _, err := ParseMarker(raw); err != nil {
		return err
	}
	pretty, err := canonjson.Canonicalize(raw)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(l.MarkerPath(), append(pretty, '\n'), 0o644)
}
