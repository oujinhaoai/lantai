package operations

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
	"github.com/oujinhaoai/lantai/internal/contract/yamljson"
	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
)

type RestoreRun struct {
	RunID          ids.ID        `json:"run_id"`
	BackupID       ids.ID        `json:"backup_id"`
	ManifestDigest digest.Digest `json:"manifest_digest"`
	Stage          string        `json:"stage"`
	SourceEpoch    int64         `json:"source_epoch"`
	StartedAt      string        `json:"started_at"`
	SourceKeyID    ids.ID        `json:"source_key_id"`
}

type RestoreOptions struct {
	Home      string
	BackupDir string
	// KeyID is obtained by the local caller from validated recovery material,
	// including a verified complete package. The key itself is not copied here.
	KeyID ids.ID
	// MinimumEpoch is the latest known live epoch when restoring an older point.
	MinimumEpoch int64
	// TargetConfig is trusted new-target configuration supplied by the local
	// recovery controller, never a path or setting selected by the archive.
	TargetConfig []byte
	// StagingDir is a private, writable recovery workspace on the target host.
	StagingDir string
	fault      func(string) error
}

// RestoreTo restores only into an empty directory (or its own interrupted copy).
// It never opens service access: the persisted restore latch survives every crash.
func RestoreTo(ctx context.Context, opts RestoreOptions) (Marker, error) {
	var zero Marker
	m, err := VerifyBackup(ctx, opts.BackupDir)
	if err != nil {
		return zero, err
	}
	if opts.KeyID != m.KeyID {
		return zero, errors.New("operations: recovery key does not match backup")
	}
	l, err := NewLayout(opts.Home)
	if err != nil {
		return zero, err
	}
	if _, err = independentTarget(opts.BackupDir, l.Home); err != nil {
		return zero, err
	}
	if err = os.MkdirAll(l.Home, 0o700); err != nil {
		return zero, err
	}
	if st, e := os.Lstat(l.Home); e != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return zero, errors.New("operations: restore destination must be a real directory")
	}
	lock, err := fsutil.TryLock(l.LockPath())
	if err != nil {
		return zero, err
	}
	defer lock.Unlock()
	raw, err := os.ReadFile(filepath.Join(opts.BackupDir, "manifest.json"))
	if err != nil {
		return zero, err
	}
	manifestDigest := digest.Of(raw)
	marker, err := ReadMarker(l)
	if errors.Is(err, fs.ErrNotExist) {
		entries, e := os.ReadDir(l.Home)
		if e != nil {
			return zero, e
		}
		for _, entry := range entries {
			if entry.Name() != "lantai.lock" {
				return zero, errors.New("operations: restore requires an empty data root")
			}
		}
		marker, err = ReadMarker(Layout{Home: opts.BackupDir})
		if err != nil {
			return zero, err
		}
		if opts.MinimumEpoch < marker.RecoveryEpoch {
			opts.MinimumEpoch = marker.RecoveryEpoch
		}
		if opts.MinimumEpoch >= 9007199254740991 {
			return zero, errors.New("operations: recovery epoch exhausted")
		}
		id := ids.New()
		marker.RecoveryEpoch = opts.MinimumEpoch + 1
		marker.UpdatedAt = clock.System{}.Now()
		marker.Restore = &RestoreRun{RunID: id, BackupID: m.BackupID, ManifestDigest: manifestDigest, Stage: "copying", SourceEpoch: m.RecoveryEpoch, StartedAt: clock.Format(marker.UpdatedAt), SourceKeyID: m.KeyID}
		if err = writeMarker(l, marker); err != nil {
			return zero, err
		}
	} else if err != nil {
		return zero, err
	} else if marker.Restore == nil || marker.Restore.BackupID != m.BackupID || marker.Restore.ManifestDigest != manifestDigest {
		return zero, errors.New("operations: destination is an existing instance or a different restore")
	}
	if opts.MinimumEpoch >= marker.RecoveryEpoch {
		return marker, errors.New("operations: resumed restore epoch is below the supplied minimum; use a new empty destination")
	}
	if marker.Restore.Stage != "copying" {
		return marker, nil
	}
	src, err := os.OpenRoot(opts.BackupDir)
	if err != nil {
		return marker, err
	}
	defer src.Close()
	dst, err := os.OpenRoot(l.Home)
	if err != nil {
		return marker, err
	}
	defer dst.Close()
	for _, file := range append(append([]BackupFile{}, m.Files...), m.Blobs...) {
		if file.Path == "instance.json" {
			continue
		}
		if _, err = copyBackupFile(ctx, src, file.Stored, dst, file.Path, &file); err != nil {
			return marker, err
		}
		if opts.fault != nil {
			if err = opts.fault("file_restored"); err != nil {
				return marker, err
			}
		}
	}
	// Preserve portable behavior and rebind only host-owned deployment fields.
	raw, err = os.ReadFile(l.ConfigPath())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return marker, err
	}
	merged, err := mergeRecoveryConfig(raw, opts.TargetConfig)
	if err != nil {
		return marker, err
	}
	if err = writeBackupJSON(l.ConfigPath(), merged); err != nil {
		return marker, err
	}
	marker.Restore.Stage = "credentials_required"
	marker.UpdatedAt = clock.System{}.Now()
	if err = writeMarker(l, marker); err != nil {
		return marker, err
	}
	return marker, nil
}

// OfflineMaintenance executes a recovery-owned local operation with no network
// listeners. It cannot be called on a running or initializing instance.
func (i *Instance) OfflineMaintenance(ctx context.Context, run func(context.Context) error) error {
	i.mu.Lock()
	if i.started || i.state == StateReady || i.state == StateInitializing || i.state == StateClosed || i.state == StateStopping {
		i.mu.Unlock()
		return errors.New("operations: offline recovery requires a stopped instance")
	}
	i.mu.Unlock()
	c, h, err := i.gate.Maintain(ctx, commands.ReasonRecovering)
	if err != nil {
		return err
	}
	defer h.Release()
	return run(c)
}

func (i *Instance) SetRestoreStage(ctx context.Context, stage string) error {
	if err := i.gate.RequireMaintenance(ctx); err != nil {
		return err
	}
	if stage != "credentials_required" && stage != "key_required" && stage != "reconciliation_required" {
		return errors.New("operations: invalid restore stage")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.marker.Restore == nil {
		return errors.New("operations: no active restore")
	}
	m := i.marker
	r := *m.Restore
	r.Stage = stage
	m.Restore = &r
	m.UpdatedAt = i.clock.Now()
	if err := writeMarker(i.layout, m); err != nil {
		return err
	}
	i.marker = m
	return nil
}

// RecoveryReview records a local operator's explicit reconciliation of changes
// newer than the chosen backup. It is bound to this exact restore and cannot be
// silently reused for another epoch or archive. Domain checks run separately.
type RecoveryReview struct {
	Contract                 string        `json:"contract"`
	RunID                    ids.ID        `json:"run_id"`
	BackupID                 ids.ID        `json:"backup_id"`
	ManifestDigest           digest.Digest `json:"manifest_digest"`
	RecoveryEpoch            int64         `json:"recovery_epoch"`
	Administrator            string        `json:"administrator"`
	RevocationsReconciled    bool          `json:"revocations_reconciled"`
	DeletionsReconciled      bool          `json:"deletions_reconciled"`
	OpenOperationsReconciled bool          `json:"open_operations_reconciled"`
	EvidenceDigest           digest.Digest `json:"evidence_digest"`
	Note                     string        `json:"note"`
	AcceptRecoveryPoint      bool          `json:"accept_recovery_point,omitempty"`
	UnknownAfterCapture      bool          `json:"unknown_after_capture,omitempty"`
}

// CompleteRestore is the last step, after application-owned credential, file and
// index checks. It leaves the gate closed; a fresh normal Start is still required.
func (i *Instance) CompleteRestore(ctx context.Context, review RecoveryReview, check func(context.Context) error) error {
	if err := i.gate.RequireMaintenance(ctx); err != nil {
		return err
	}
	m := i.Marker()
	if m.Restore == nil && review.RunID.Valid() && review.RecoveryEpoch == m.RecoveryEpoch {
		var previous RecoveryReview
		if err := readBackupJSON(filepath.Join(i.layout.Home, "logs", "restore-"+string(review.RunID)+".json"), &previous); err == nil && previous == review {
			return nil
		}
		return errors.New("operations: no matching completed restore receipt")
	}
	r := m.Restore
	legacyReview := review.Contract == "lantai.recovery-review/v1" && review.RevocationsReconciled && review.DeletionsReconciled && review.OpenOperationsReconciled && !review.AcceptRecoveryPoint && !review.UnknownAfterCapture
	pointReview := review.Contract == "lantai.recovery-review/v2" && review.AcceptRecoveryPoint && review.UnknownAfterCapture && !review.RevocationsReconciled && !review.DeletionsReconciled && !review.OpenOperationsReconciled
	if r == nil || r.Stage != "reconciliation_required" || (!legacyReview && !pointReview) || review.RunID != r.RunID || review.BackupID != r.BackupID || review.ManifestDigest != r.ManifestDigest || review.RecoveryEpoch != m.RecoveryEpoch || review.Administrator == "" || !review.EvidenceDigest.Valid() || len(review.Note) < 8 || len(review.Note) > 2000 {
		return errors.New("operations: complete, bound reconciliation review is required")
	}
	if check == nil {
		return errors.New("operations: restore requires domain validation")
	}
	if err := check(ctx); err != nil {
		return err
	}
	if err := writeBackupJSON(filepath.Join(i.layout.Home, "logs", "restore-"+string(r.RunID)+".json"), review); err != nil {
		return err
	}
	m.Restore = nil
	m.UpdatedAt = i.clock.Now()
	if err := writeMarker(i.layout, m); err != nil {
		return err
	}
	i.mu.Lock()
	i.marker = m
	i.mu.Unlock()
	if err := i.reevaluate(ctx); err != nil {
		return err
	}
	if len(i.Readiness().Reasons) > 0 {
		return fmt.Errorf("operations: restore finished but instance remains blocked: %v", i.Readiness().Reasons)
	}
	return nil
}

// mergeRecoveryConfig retains source quotas, expiry, throttling, HTTP limits,
// instance name and lifecycle tuning. A new host supplies only deployment
// bindings. Missing bindings deliberately select safe built-in defaults.
func mergeRecoveryConfig(source, target []byte) (map[string]any, error) {
	decode := func(raw []byte) (map[string]any, error) {
		if len(raw) == 0 {
			return map[string]any{"contract": ConfigContract}, nil
		}
		doc, err := yamljson.Decode(raw)
		if err != nil {
			return nil, err
		}
		reg, err := schema.Default()
		if err != nil {
			return nil, err
		}
		if err = reg.Validate(ConfigContract, doc); err != nil {
			return nil, err
		}
		return doc.(map[string]any), nil
	}
	original, err := decode(source)
	if err != nil {
		return nil, err
	}
	binding, err := decode(target)
	if err != nil {
		return nil, err
	}
	object := func(m map[string]any, k string) map[string]any {
		if v, ok := m[k].(map[string]any); ok {
			return v
		}
		v := map[string]any{}
		m[k] = v
		return v
	}
	original["secrets"] = map[string]any{"dir": "secrets"}
	if v, ok := binding["secrets"]; ok {
		original["secrets"] = v
	}
	delete(original, "listen")
	if v, ok := binding["listen"]; ok {
		original["listen"] = v
	}
	httpCfg := object(original, "http")
	delete(httpCfg, "allowed_origins")
	if v, ok := object(binding, "http")["allowed_origins"]; ok {
		httpCfg["allowed_origins"] = v
	}
	// FUSE admission is a property of this host, never inherited permission.
	object(original, "storage")["allow_fuse"] = false
	if v, ok := object(binding, "storage")["allow_fuse"]; ok {
		object(original, "storage")["allow_fuse"] = v
	}
	object(original, "lifecycle")["scheduler"] = false
	return original, nil
}
