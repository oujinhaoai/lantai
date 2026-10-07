package operations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/contract/pin"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite/migrations"
)

const BackupContract = "lantai.backup/v1"

type BackupManifest struct {
	Contract      string           `json:"contract"`
	BackupID      ids.ID           `json:"backup_id"`
	InstanceID    ids.ID           `json:"instance_id"`
	RecoveryEpoch int64            `json:"recovery_epoch"`
	State         string           `json:"state"`
	CreatedAt     time.Time        `json:"created_at"`
	CompletedAt   time.Time        `json:"completed_at,omitempty"`
	Matrix        Matrix           `json:"matrix"`
	KeyID         ids.ID           `json:"key_id"`
	Watermarks    map[string]int64 `json:"watermarks"`
	Files         []BackupFile     `json:"files"`
	Blobs         []BackupFile     `json:"blobs"`
}

// MarshalJSON uses the shared fixed UTC-millisecond timestamp convention.
func (m BackupManifest) MarshalJSON() ([]byte, error) {
	type alias BackupManifest
	end := ""
	if !m.CompletedAt.IsZero() {
		end = clock.Format(m.CompletedAt)
	}
	return json.Marshal(struct {
		alias
		Created   string `json:"created_at"`
		Completed string `json:"completed_at,omitempty"`
	}{alias(m), clock.Format(m.CreatedAt), end})
}

type backupRecord struct {
	Manifest           BackupManifest `json:"manifest"`
	Destination        string         `json:"destination"`
	PinReleasedAt      time.Time      `json:"pin_released_at,omitempty"`
	VerifiedAt         time.Time      `json:"verified_at,omitempty"`
	RetiredDestination string         `json:"retired_destination,omitempty"`
}

type BackupOptions struct {
	// Destination is an independent, initially empty directory. BackupID is
	// required for resumption; it never captures a new point under a copying ID.
	Destination string
	BackupID    ids.ID
	KeyID       ids.ID
	// Check runs under the common exclusive barrier, before any snapshot. It
	// is the application owner's deep domain integrity check.
	Check      func(context.Context) error
	Watermarks func(context.Context) (map[string]int64, error)
	// Fault is internal test-only injection after durable protocol boundaries.
	fault func(string) error
}

func (i *Instance) backupRecordPath(id ids.ID) string {
	return filepath.Join(i.layout.Home, "backups", "records", string(id)+".json")
}

// Backup captures all four authoritative databases and mutable files under one
// write barrier. Only immutable CAS copying occurs after the barrier is released.
// Its durable source record is also the backup pin; errors retain it for Resume.
func (i *Instance) Backup(ctx context.Context, opts BackupOptions) (BackupManifest, error) {
	release, err := i.lockBackup(ctx)
	if err != nil {
		return BackupManifest{}, err
	}
	defer release()
	if st := i.State(); st == StateStopping || st == StateClosed {
		return BackupManifest{}, errors.New("operations: instance is closing")
	}
	if opts.BackupID != "" {
		if !opts.BackupID.Valid() {
			return BackupManifest{}, errors.New("operations: invalid backup ID")
		}
		var rec backupRecord
		if err := readBackupJSON(i.backupRecordPath(opts.BackupID), &rec); err != nil {
			return BackupManifest{}, err
		}
		if opts.Destination != "" {
			abs, err := filepath.Abs(opts.Destination)
			if err != nil || abs != rec.Destination {
				return rec.Manifest, errors.New("operations: resume destination differs from the original backup")
			}
		}
		if rec.Manifest.State == "capturing" {
			if err := i.resumeCapture(ctx, &rec, opts); err != nil {
				return rec.Manifest, err
			}
		}
		return i.finishBackup(ctx, &rec, opts.fault)
	}
	if !opts.KeyID.Valid() {
		return BackupManifest{}, errors.New("operations: backup requires the separately recoverable master key ID")
	}
	target, err := independentTarget(i.layout.Home, opts.Destination)
	if err != nil {
		return BackupManifest{}, err
	}
	if opts.Destination == "" {
		return BackupManifest{}, errors.New("operations: backup destination is required")
	}
	if err = ensureEmptyDirectory(target); err != nil {
		return BackupManifest{}, err
	}
	id, err := i.ids.New()
	if err != nil {
		return BackupManifest{}, err
	}
	rec := backupRecord{Destination: target, Manifest: BackupManifest{Contract: BackupContract, BackupID: id, State: "capturing", CreatedAt: i.clock.Now(), KeyID: opts.KeyID, Files: []BackupFile{}, Blobs: []BackupFile{}, Watermarks: map[string]int64{}}}
	if err = writeBackupJSON(filepath.Join(target, "manifest.json"), rec.Manifest); err != nil {
		return rec.Manifest, err
	}
	if err = writeBackupJSON(i.backupRecordPath(id), rec); err != nil {
		return rec.Manifest, err
	}
	if err = i.captureBackup(ctx, &rec, opts); err != nil {
		return rec.Manifest, err
	}
	return i.finishBackup(ctx, &rec, opts.fault)
}

func ensureEmptyDirectory(dir string) error {
	if st, err := os.Lstat(dir); err == nil {
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return errors.New("operations: destination must be an empty real directory")
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return errors.New("operations: destination is not empty")
		}
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.MkdirAll(dir, 0o700)
}

// captureWindow also supports a stopped instance awaiting compatible migration:
// an upgrade must be able to back up the old schema before changing it. It never
// opens that instance's gate or bypasses a recorded incomplete migration.
func (i *Instance) captureWindow(ctx context.Context) (context.Context, func(), error) {
	i.mu.Lock()
	old := i.state
	if i.marker.State != MarkerActive || i.marker.Migration != nil || i.marker.Restore != nil || old == StateClosed || old == StateStopping || old == StateMaintenance || old == StateRecovering {
		i.mu.Unlock()
		return ctx, nil, errors.New("operations: instance cannot capture a backup in its current state")
	}
	i.state = StateMaintenance
	i.mu.Unlock()
	c, h, err := i.gate.Maintain(ctx, "backup")
	restore := func() {
		i.mu.Lock()
		defer i.mu.Unlock()
		if i.state == StateMaintenance {
			i.state = old
			if old == StateReady {
				i.gate.Open()
			}
		}
	}
	if err != nil {
		restore()
		return ctx, nil, err
	}
	return c, func() { restore(); h.Release() }, nil
}

func (i *Instance) captureBackup(ctx context.Context, rec *backupRecord, opts BackupOptions) error {
	c, end, err := i.captureWindow(ctx)
	if err != nil {
		return err
	}
	defer end()
	if opts.Check != nil {
		if err = opts.Check(c); err != nil {
			return err
		}
	}
	for _, base := range []string{"projects", "trash", "staging", "quarantine", "audit", "logs"} {
		area := filepath.Join(i.layout.Home, base)
		if _, e := independentTarget(area, i.cfg.SecretsDir); e != nil {
			return errors.New("operations: secret directory overlaps a captured file area")
		}
	}
	m := &rec.Manifest
	marker := i.Marker()
	m.InstanceID = marker.InstanceID
	m.RecoveryEpoch = marker.RecoveryEpoch
	m.Matrix = Matrix{DataFormatVersion: marker.DataFormatVersion, Databases: map[ownership.Database]int{}}
	if opts.Watermarks != nil {
		m.Watermarks, err = opts.Watermarks(c)
		if err != nil {
			return err
		}
	}
	src, err := os.OpenRoot(i.layout.Home)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenRoot(rec.Destination)
	if err != nil {
		return err
	}
	defer dst.Close()
	// Enumerate and hash immutable roots while writers are stopped. Persist the
	// full pin before taking snapshots and before allowing a future GC to run.
	err = fs.WalkDir(src.FS(), "blobs", func(p string, d fs.DirEntry, e error) error {
		if errors.Is(e, fs.ErrNotExist) && p == "blobs" {
			return fs.SkipDir
		}
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		f, e := hashBackupFile(c, src, p)
		if e != nil {
			return e
		}
		sha := path.Base(p)
		if len(sha) != 64 || f.SHA256 != sha || p != "blobs/sha256/"+sha[:2]+"/"+sha[2:4]+"/"+sha {
			return errors.New("operations: invalid CAS entry")
		}
		m.Blobs = append(m.Blobs, f)
		return nil
	})
	if err != nil {
		return err
	}
	if err = writeBackupJSON(i.backupRecordPath(m.BackupID), rec); err != nil {
		return err
	}
	if opts.fault != nil {
		if err = opts.fault("pin_persisted"); err != nil {
			return err
		}
	}
	if err = dst.MkdirAll("db", 0o700); err != nil {
		return err
	}
	for _, name := range authoritative {
		file := "db/" + string(name) + ".db"
		target := filepath.Join(rec.Destination, filepath.FromSlash(file))
		// VACUUM INTO includes WAL-visible committed pages and emits a standalone
		// snapshot; four invocations share the same process-wide write barrier.
		if _, err = i.DB(name).ExecContext(c, "VACUUM INTO ?", target); err != nil {
			return err
		}
		if err = os.Chmod(target, 0o600); err != nil {
			return err
		}
		frozen, err := os.OpenFile(target, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		syncErr := frozen.Sync()
		closeErr := frozen.Close()
		if err = errors.Join(syncErr, closeErr); err != nil {
			return err
		}
		f, err := hashBackupFile(c, dst, file)
		if err != nil {
			return err
		}
		st, err := verifyBackupDB(c, target, name, marker.InstanceID, i.src)
		if err != nil {
			return err
		}
		m.Matrix.Databases[name] = st.Applied
		m.Files = append(m.Files, f)
	}
	if err = fsutil.SyncDir(filepath.Join(rec.Destination, "db")); err != nil {
		return err
	}
	// These are file-owner roots, not a recursive copy of the instance: secrets,
	// lock files, index.db, and previous backups can never enter the archive.
	for _, base := range []string{"instance.json", "config.yaml", "projects", "trash", "staging", "quarantine", "audit", "logs"} {
		err = fs.WalkDir(src.FS(), base, func(p string, d fs.DirEntry, e error) error {
			if errors.Is(e, fs.ErrNotExist) && p == base {
				return nil
			}
			if e != nil {
				return e
			}
			if d.IsDir() {
				return nil
			}
			f, e := copyBackupFile(c, src, p, dst, p, nil)
			if e != nil {
				return e
			}
			m.Files = append(m.Files, f)
			return nil
		})
		if err != nil {
			return err
		}
	}
	slices.SortFunc(m.Files, func(a, b BackupFile) int { return strings.Compare(a.Path, b.Path) })
	m.State = "copying"
	if err = writeBackupJSON(filepath.Join(rec.Destination, "manifest.json"), m); err != nil {
		return err
	}
	if err = writeBackupJSON(i.backupRecordPath(m.BackupID), rec); err != nil {
		return err
	}
	if opts.fault != nil {
		return opts.fault("common_point_captured")
	}
	return nil
}

func verifyBackupDB(ctx context.Context, file string, name ownership.Database, instance ids.ID, source migrationSource) (DBStatus, error) {
	db, err := sqlite.Open(ctx, file, sqlite.Options{ReadOnly: true})
	if err != nil {
		return DBStatus{}, err
	}
	defer db.Close()
	var integrity string
	if err = db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return DBStatus{}, err
	}
	if integrity != "ok" {
		return DBStatus{}, errors.New("operations: backup database integrity failed")
	}
	st, err := inspectDB(ctx, db, name, source)
	if err != nil {
		return st, err
	}
	if st.InstanceID != instance || len(st.Problems) > 0 {
		return st, errors.New("operations: backup database binding or migration history is invalid")
	}
	return st, nil
}

func (i *Instance) finishBackup(ctx context.Context, rec *backupRecord, fault func(string) error) (BackupManifest, error) {
	m := &rec.Manifest
	if m.InstanceID != i.InstanceID() {
		return *m, errors.New("operations: backup belongs to a different instance")
	}
	if m.State != "copying" && m.State != "complete" {
		return *m, errors.New("operations: backup is not resumable")
	}
	if m.State == "complete" {
		v, err := VerifyBackup(ctx, rec.Destination)
		return v, err
	}
	// A durable complete marker may precede releasing the source pin after a
	// crash. Verify it, then finish the same source record; never recapture.
	if done, err := VerifyBackup(ctx, rec.Destination); err == nil {
		if done.BackupID != m.BackupID {
			return *m, errors.New("operations: destination contains another backup")
		}
		*m = done
		rec.PinReleasedAt = i.clock.Now()
		rec.VerifiedAt = i.clock.Now()
		return *m, writeBackupJSON(i.backupRecordPath(m.BackupID), rec)
	}
	src, err := os.OpenRoot(i.layout.Home)
	if err != nil {
		return *m, err
	}
	defer src.Close()
	dst, err := os.OpenRoot(rec.Destination)
	if err != nil {
		return *m, err
	}
	defer dst.Close()
	for _, f := range m.Blobs {
		if got, e := hashBackupFile(ctx, dst, f.Stored); e == nil && got.SHA256 == f.SHA256 && got.Size == f.Size {
			continue
		}
		if _, err = copyBackupFile(ctx, src, f.Path, dst, f.Stored, &f); err != nil {
			return *m, err
		}
		if fault != nil {
			if err = fault("blob_copied"); err != nil {
				return *m, err
			}
		}
	}
	if err = verifyBackupContents(ctx, rec.Destination, *m); err != nil {
		return *m, err
	}
	m.State = "complete"
	m.CompletedAt = i.clock.Now()
	if err = writeBackupJSON(filepath.Join(rec.Destination, "manifest.json"), m); err != nil {
		return *m, err
	}
	raw, err := os.ReadFile(filepath.Join(rec.Destination, "manifest.json"))
	if err != nil {
		return *m, err
	}
	if err = fsutil.WriteFileAtomic(filepath.Join(rec.Destination, "COMPLETE"), []byte(string(digest.Of(raw))+"\n"), 0o600); err != nil {
		return *m, err
	}
	if fault != nil {
		if err = fault("complete_marked"); err != nil {
			return *m, err
		}
	}
	rec.PinReleasedAt = i.clock.Now()
	rec.VerifiedAt = i.clock.Now()
	return *m, writeBackupJSON(i.backupRecordPath(m.BackupID), rec)
}

// VerifyBackup is read-only and does not trust a state field by itself. The
// completion marker binds the exact manifest, and every entry is rehashed.
func VerifyBackup(ctx context.Context, dir string) (BackupManifest, error) {
	var m BackupManifest
	info, err := os.Lstat(dir)
	if err != nil {
		return m, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return m, errors.New("operations: backup root must be a real directory")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return m, err
	}
	defer root.Close()
	for _, name := range []string{"manifest.json", "COMPLETE"} {
		if err = regularPath(root, name); err != nil {
			return m, err
		}
	}
	file, err := root.Open("manifest.json")
	if err != nil {
		return m, err
	}
	raw, err := decodeBackupJSON(file, &m)
	_ = file.Close()
	if err != nil {
		return m, err
	}
	if m.State != "complete" || m.CompletedAt.IsZero() {
		return m, errors.New("operations: backup is incomplete")
	}
	file, err = root.Open("COMPLETE")
	if err != nil {
		return m, err
	}
	complete, err := io.ReadAll(io.LimitReader(file, 129))
	_ = file.Close()
	if err != nil {
		return m, err
	}
	if len(complete) > 128 {
		return m, errors.New("operations: invalid complete marker")
	}
	if strings.TrimSpace(string(complete)) != string(digest.Of(raw)) {
		return m, errors.New("operations: complete marker does not match manifest")
	}
	return m, verifyBackupContents(ctx, dir, m)
}

func verifyBackupContents(ctx context.Context, dir string, m BackupManifest) error {
	raw, err := json.Marshal(m)
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
	if err = reg.Validate(BackupContract, doc); err != nil {
		return err
	}
	if m.Contract != BackupContract || !m.BackupID.Valid() || !m.InstanceID.Valid() || !m.KeyID.Valid() || m.RecoveryEpoch < 1 || m.CreatedAt.IsZero() {
		return errors.New("operations: invalid backup identity")
	}
	if m.Matrix.DataFormatVersion > DataFormatVersion || m.Matrix.DataFormatVersion < 1 {
		return errors.New("operations: unsupported backup format")
	}
	if len(m.Matrix.Databases) != len(authoritative) {
		return errors.New("operations: backup matrix must contain exactly four authoritative databases")
	}
	for _, f := range m.Blobs {
		if !validBackupBlob(f) {
			return errors.New("operations: invalid backup CAS entry")
		}
	}
	for _, f := range m.Files {
		if strings.HasPrefix(f.Path, "blobs/") {
			return errors.New("operations: CAS entries must be in the pinned blob inventory")
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	seen := map[string]bool{}
	for _, f := range append(slices.Clone(m.Files), m.Blobs...) {
		if !safeRelative(f.Path) || f.Stored != f.Path || seen[strings.ToLower(f.Path)] || f.Size < 0 || !digest.Digest("sha256:"+f.SHA256).Valid() {
			return errors.New("operations: invalid or duplicate backup entry")
		}
		seen[strings.ToLower(f.Path)] = true
		first := strings.Split(f.Path, "/")[0]
		if !slices.Contains([]string{"db", "instance.json", "config.yaml", "projects", "trash", "staging", "quarantine", "audit", "logs", "blobs"}, first) {
			return errors.New("operations: backup contains a forbidden file area")
		}
		if first == "db" && !slices.Contains([]string{"db/main.db", "db/ledger.db", "db/runtime.db", "db/events.db"}, f.Path) {
			return errors.New("operations: invalid database entry")
		}
		got, err := hashBackupFile(ctx, root, f.Stored)
		if err != nil {
			return err
		}
		if got.SHA256 != f.SHA256 || got.Size != f.Size {
			return fmt.Errorf("operations: backup file checksum mismatch: %s", f.Path)
		}
	}
	if !seen["instance.json"] {
		return errors.New("operations: backup has no instance marker")
	}
	marker, err := ReadMarker(Layout{Home: dir})
	if err != nil {
		return err
	}
	if marker.InstanceID != m.InstanceID || marker.RecoveryEpoch != m.RecoveryEpoch || marker.State != MarkerActive || marker.Migration != nil || marker.Restore != nil || marker.DataFormatVersion != m.Matrix.DataFormatVersion {
		return errors.New("operations: backup marker does not match the common point")
	}
	for _, name := range authoritative {
		p := "db/" + string(name) + ".db"
		if !seen[p] {
			return errors.New("operations: backup is missing an authoritative database")
		}
		st, err := verifyBackupDB(ctx, filepath.Join(dir, filepath.FromSlash(p)), name, m.InstanceID, migrations.For)
		if err != nil {
			return err
		}
		if st.Applied != m.Matrix.Databases[name] {
			return errors.New("operations: backup schema matrix does not match database")
		}
	}
	return nil
}

func (i *Instance) backupRecords(ctx context.Context) ([]backupRecord, error) {
	entries, err := os.ReadDir(filepath.Join(i.layout.Home, "backups", "records"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []backupRecord
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var r backupRecord
		if err = readBackupJSON(filepath.Join(i.layout.Home, "backups", "records", e.Name()), &r); err != nil {
			return nil, err
		}
		if r.Manifest.InstanceID != "" && r.Manifest.InstanceID != i.InstanceID() {
			return nil, errors.New("operations: foreign backup pin record")
		}
		out = append(out, r)
	}
	return out, nil
}

func (i *Instance) PinsFor(ctx context.Context, sha string) ([]pin.Pin, error) {
	records, err := i.backupRecords(ctx)
	if err != nil {
		return nil, err
	}
	var out []pin.Pin
	for _, r := range records {
		blobs := make([]string, 0, len(r.Manifest.Blobs))
		for _, b := range r.Manifest.Blobs {
			blobs = append(blobs, b.SHA256)
		}
		slices.Sort(blobs)
		blobs = slices.Compact(blobs)
		if slices.Contains(blobs, sha) {
			out = append(out, pin.Pin{PinID: r.Manifest.BackupID, Kind: pin.KindBackup, OwnerModule: "operations", Owner: pin.Owner{Kind: "backup", ID: r.Manifest.BackupID}, Blobs: blobs, CreatedAt: r.Manifest.CreatedAt, ReleasedAt: r.PinReleasedAt})
		}
	}
	return out, nil
}

// CancelBackup serializes with a copier, so pins are never released while bytes
// are still being copied. It retains incomplete artifacts for diagnosis.
func (i *Instance) CancelBackup(ctx context.Context, id ids.ID) error {
	release, err := i.lockBackup(ctx)
	if err != nil {
		return err
	}
	defer release()
	if st := i.State(); st == StateStopping || st == StateClosed {
		return errors.New("operations: instance is closing")
	}
	if !id.Valid() {
		return errors.New("operations: invalid backup ID")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var r backupRecord
	if err := readBackupJSON(i.backupRecordPath(id), &r); err != nil {
		return err
	}
	if r.Manifest.State == "complete" {
		return errors.New("operations: a complete backup cannot be cancelled")
	}
	r.Manifest.State = "cancelled"
	r.PinReleasedAt = i.clock.Now()
	return writeBackupJSON(i.backupRecordPath(id), r)
}

type BackupStats struct {
	Complete, Incomplete, Pins int64
	LastComplete, LastVerified time.Time
}

func (i *Instance) BackupStats(ctx context.Context) (BackupStats, error) {
	var s BackupStats
	records, err := i.backupRecords(ctx)
	if err != nil {
		return s, err
	}
	for _, r := range records {
		if r.Manifest.State == "complete" {
			s.Complete++
			if r.Manifest.CompletedAt.After(s.LastComplete) {
				s.LastComplete = r.Manifest.CompletedAt
			}
		} else if r.Manifest.State != "cancelled" {
			s.Incomplete++
		}
		if r.PinReleasedAt.IsZero() {
			s.Pins++
		}
		if r.VerifiedAt.After(s.LastVerified) {
			s.LastVerified = r.VerifiedAt
		}
	}
	return s, nil
}

var _ pin.Source = (*Instance)(nil)

func validBackupBlob(f BackupFile) bool {
	sha := f.SHA256
	return digest.Digest("sha256:"+sha).Valid() && f.Path == "blobs/sha256/"+sha[:2]+"/"+sha[2:4]+"/"+sha && f.Stored == f.Path
}

// A reset journal survives every rename/recreate boundary. Once the destination
// published a copying manifest, that frozen point is adopted and never replaced.
func (i *Instance) resumeCapture(ctx context.Context, rec *backupRecord, opts BackupOptions) error {
	if rec.Manifest.InstanceID != "" && rec.Manifest.InstanceID != i.InstanceID() {
		return errors.New("operations: foreign backup capture")
	}
	if _, err := independentTarget(i.layout.Home, rec.Destination); err != nil {
		return err
	}
	if rec.RetiredDestination == "" {
		var prior BackupManifest
		err := readBackupJSON(filepath.Join(rec.Destination, "manifest.json"), &prior)
		if err != nil {
			return err
		}
		if prior.BackupID != rec.Manifest.BackupID || prior.KeyID != rec.Manifest.KeyID {
			return errors.New("operations: interrupted capture destination changed")
		}
		if prior.State == "copying" || prior.State == "complete" {
			// Keep the source pin held until full verification and COMPLETE.
			prior.State = "copying"
			rec.Manifest = prior
			return writeBackupJSON(i.backupRecordPath(prior.BackupID), rec)
		}
		if prior.State != "capturing" {
			return errors.New("operations: capture cannot resume from this destination state")
		}
		attempt, err := i.ids.New()
		if err != nil {
			return err
		}
		rec.RetiredDestination = rec.Destination + ".incomplete-" + string(attempt)
		if err = writeBackupJSON(i.backupRecordPath(rec.Manifest.BackupID), rec); err != nil {
			return err
		}
	}
	if !strings.HasPrefix(rec.RetiredDestination, rec.Destination+".incomplete-") || !ids.ID(strings.TrimPrefix(rec.RetiredDestination, rec.Destination+".incomplete-")).Valid() {
		return errors.New("operations: invalid capture reset journal")
	}
	if _, err := os.Lstat(rec.RetiredDestination); errors.Is(err, fs.ErrNotExist) {
		if err = os.Rename(rec.Destination, rec.RetiredDestination); err != nil {
			return err
		}
		if err = fsutil.SyncDir(filepath.Dir(rec.Destination)); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := ensureEmptyDirectory(rec.Destination); err != nil {
		entries, e := os.ReadDir(rec.Destination)
		var current BackupManifest
		if e != nil || len(entries) != 1 || entries[0].Name() != "manifest.json" || readBackupJSON(filepath.Join(rec.Destination, "manifest.json"), &current) != nil || current.BackupID != rec.Manifest.BackupID || current.State != "capturing" {
			return err
		}
	}
	rec.Manifest.Files = []BackupFile{}
	rec.Manifest.Blobs = []BackupFile{}
	rec.Manifest.CreatedAt = i.clock.Now()
	if err := writeBackupJSON(filepath.Join(rec.Destination, "manifest.json"), rec.Manifest); err != nil {
		return err
	}
	rec.RetiredDestination = ""
	if err := writeBackupJSON(i.backupRecordPath(rec.Manifest.BackupID), rec); err != nil {
		return err
	}
	return i.captureBackup(ctx, rec, opts)
}

// Backup copying outlives the common capture barrier, but never the instance
// lock. Shutdown and cancellation wait with their own bounded contexts.
func (i *Instance) lockBackup(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	i.backupOnce.Do(func() { i.backupLock = make(chan struct{}, 1) })
	select {
	case i.backupLock <- struct{}{}:
		return func() { <-i.backupLock }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
