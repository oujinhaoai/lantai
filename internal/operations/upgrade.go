package operations

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
)

// Migrate requires a verified common-point backup before a new upgrade. The
// durable migration record retains its binding so an interrupted run can resume
// even while the backup medium is offline. Downgrades are never attempted.
func (i *Instance) Migrate(ctx context.Context, backupDirs ...string) (MigrationReport, error) {
	if len(backupDirs) > 1 {
		return MigrationReport{}, errors.New("operations: at most one backup is accepted")
	}
	state := i.State()
	if state != StateStarting && state != StateBlocked {
		return MigrationReport{}, errors.New("operations: migrate requires a stopped instance")
	}
	m := i.Marker()
	pending := m.Migration != nil || m.DataFormatVersion < DataFormatVersion
	for _, s := range i.statusesCopy() {
		pending = pending || s.Pending()
	}
	if !pending {
		return i.migrate(ctx)
	}
	if len(backupDirs) == 0 || backupDirs[0] == "" {
		if m.Migration != nil && m.Migration.BackupID.Valid() && m.Migration.BackupDigest.Valid() {
			return i.migrate(ctx)
		}
		return MigrationReport{}, errors.New("operations: upgrade requires a verified complete backup; supply -backup")
	}
	b, err := VerifyBackup(ctx, backupDirs[0])
	if err != nil {
		return MigrationReport{}, err
	}
	raw, err := os.ReadFile(filepath.Join(backupDirs[0], "manifest.json"))
	if err != nil {
		return MigrationReport{}, err
	}
	sum := digest.Of(raw)
	if b.InstanceID != m.InstanceID {
		return MigrationReport{}, errors.New("operations: upgrade backup belongs to another instance")
	}
	if m.Restore != nil {
		if b.BackupID != m.Restore.BackupID || sum != m.Restore.ManifestDigest {
			return MigrationReport{}, errors.New("operations: restored upgrade requires its original complete backup")
		}
	} else if b.RecoveryEpoch != m.RecoveryEpoch {
		return MigrationReport{}, errors.New("operations: upgrade backup belongs to another recovery epoch")
	}
	if m.Migration != nil && m.Migration.BackupID != "" && (m.Migration.BackupID != b.BackupID || m.Migration.BackupDigest != sum) {
		return MigrationReport{}, errors.New("operations: migration backup cannot be replaced")
	}
	for _, s := range i.statusesCopy() {
		if s.Database == "index" {
			continue
		}
		v, ok := b.Matrix.Databases[s.Database]
		if !ok || v > s.Applied || (m.Migration == nil && v != s.Applied) {
			return MigrationReport{}, errors.New("operations: upgrade backup does not match the pre-upgrade schema")
		}
	}
	if m.Migration == nil {
		id, err := i.ids.New()
		if err != nil {
			return MigrationReport{}, err
		}
		m.Migration = &MigrationRun{RunID: id, StartedAt: i.clock.Now(), FromFormatVersion: m.DataFormatVersion, TargetFormatVersion: DataFormatVersion}
	} else {
		r := *m.Migration
		m.Migration = &r
	}
	m.Migration.BackupID = b.BackupID
	m.Migration.BackupDigest = sum
	m.UpdatedAt = i.clock.Now()
	// Persist the binding only while this instance still owns a stopped,
	// exclusive maintenance window. A concurrent Close or migration cannot race
	// the journal replacement after the (potentially long) archive verification.
	_, held, err := i.gate.Maintain(ctx, commands.ReasonMigrating)
	if err != nil {
		return MigrationReport{}, err
	}
	i.mu.Lock()
	if i.state != StateStarting && i.state != StateBlocked {
		i.mu.Unlock()
		held.Release()
		return MigrationReport{}, errors.New("operations: instance changed during upgrade verification")
	}
	current := i.marker.Migration
	if current != nil && current.RunID != m.Migration.RunID {
		i.mu.Unlock()
		held.Release()
		return MigrationReport{}, errors.New("operations: another migration was recorded during verification")
	}
	err = writeMarker(i.layout, m)
	if err == nil {
		i.marker = m
	}
	i.mu.Unlock()
	held.Release()
	if err != nil {
		return MigrationReport{}, err
	}
	return i.migrate(ctx)
}
