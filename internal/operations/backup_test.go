package operations

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite"
)

var errBackupCrash = errors.New("injected durable boundary crash")

func backupInstance(t *testing.T) *Instance {
	t.Helper()
	i := open(t, createActive(t, Options{}), Options{})
	if err := i.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	return i
}
func backupWrite(t *testing.T, home, path string, b []byte) {
	t.Helper()
	p := filepath.Join(home, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
}
func backupBlob(t *testing.T, i *Instance, b []byte) (string, string) {
	t.Helper()
	sha := strings.TrimPrefix(string(digest.Of(b)), "sha256:")
	p := "blobs/sha256/" + sha[:2] + "/" + sha[2:4] + "/" + sha
	backupWrite(t, i.Layout().Home, p, b)
	return sha, p
}
func assertBackupPin(t *testing.T, i *Instance, sha string, active bool) {
	t.Helper()
	pins, err := i.PinsFor(t.Context(), sha)
	if err != nil || len(pins) != 1 {
		t.Fatalf("pins=%+v err=%v", pins, err)
	}
	if err := pins[0].Validate(); err != nil {
		t.Fatal(err)
	}
	if pins[0].ActiveAt(time.Now()) != active {
		t.Fatalf("pin active=%v expected %v", pins[0].ActiveAt(time.Now()), active)
	}
}

func TestBackupCommonBarrierWALAndFrozenMutableFiles(t *testing.T) {
	i := backupInstance(t)
	ctx := t.Context()
	dest := filepath.Join(t.TempDir(), "archive")
	sha, _ := backupBlob(t, i, []byte("original blob"))
	probe := map[ownership.Database]string{ownership.Main: "identity_backup_probe", ownership.Ledger: "ledger_backup_probe", ownership.Runtime: "storage_backup_probe", ownership.Events: "events_backup_probe"}
	for db, table := range probe {
		if _, err := i.DB(db).ExecContext(ctx, "CREATE TABLE "+table+" (value INTEGER NOT NULL)"); err != nil {
			t.Fatal(err)
		}
		if _, err := i.DB(db).ExecContext(ctx, "INSERT INTO "+table+" VALUES (0)"); err != nil {
			t.Fatal(err)
		}
	}
	_, held, err := i.Gate().Acquire(ctx, commands.Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	for db, table := range probe {
		if _, err := i.DB(db).ExecContext(ctx, "UPDATE "+table+" SET value=1"); err != nil {
			t.Fatal(err)
		}
	}
	backupWrite(t, i.Layout().Home, "projects/example/asset.yaml", []byte("revision: 1\n"))
	backupWrite(t, i.Layout().Home, "secrets/not-backed-up", []byte("private-key-material"))
	type result struct {
		m   BackupManifest
		err error
	}
	done := make(chan result, 1)
	checked := make(chan struct{})
	go func() {
		m, err := i.Backup(ctx, BackupOptions{Destination: dest, KeyID: ids.New(), Check: func(c context.Context) error {
			if err := i.Gate().RequireMaintenance(c); err != nil {
				return err
			}
			close(checked)
			return nil
		}, Watermarks: func(context.Context) (map[string]int64, error) { return map[string]int64{"events": 7}, nil }, fault: func(stage string) error {
			if stage == "common_point_captured" {
				return errBackupCrash
			}
			return nil
		}})
		done <- result{m, err}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for i.State() != StateMaintenance {
		if time.Now().After(deadline) {
			t.Fatal("backup did not close write gate")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-checked:
		t.Fatal("backup did not wait for in-flight writer")
	default:
	}
	held.Release()
	res := <-done
	if !errors.Is(res.err, errBackupCrash) || res.m.State != "copying" {
		t.Fatalf("capture=%+v %v", res.m, res.err)
	}
	assertBackupPin(t, i, sha, true)
	if !i.Readiness().Ready {
		t.Fatal("backup did not restore readiness")
	}
	for db, table := range probe {
		wal, err := os.Stat(i.Layout().DBPath(db) + "-wal")
		if err != nil || wal.Size() == 0 {
			t.Fatalf("test lacked WAL: %s %v", db, err)
		}
		if _, err := i.DB(db).ExecContext(ctx, "UPDATE "+table+" SET value=2"); err != nil {
			t.Fatal(err)
		}
	}
	backupWrite(t, i.Layout().Home, "projects/example/asset.yaml", []byte("revision: 2\n"))
	m, err := i.Backup(ctx, BackupOptions{BackupID: res.m.BackupID, Destination: dest, Check: func(context.Context) error { return errors.New("must not recapture") }})
	if err != nil || m.State != "complete" || m.Watermarks["events"] != 7 {
		t.Fatalf("resume=%+v %v", m, err)
	}
	for db, table := range probe {
		snap, err := sqlite.Open(ctx, filepath.Join(dest, "db", string(db)+".db"), sqlite.Options{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		var value int
		err = snap.QueryRowContext(ctx, "SELECT value FROM "+table).Scan(&value)
		snap.Close()
		if err != nil || value != 1 {
			t.Fatalf("%s snapshot=%d %v", db, value, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(dest, "projects/example/asset.yaml"))
	if err != nil || string(data) != "revision: 1\n" {
		t.Fatalf("mutable snapshot changed: %s %v", data, err)
	}
	for _, excluded := range []string{"db/index.db", "secrets/not-backed-up", "lantai.lock"} {
		if _, err := os.Stat(filepath.Join(dest, excluded)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("excluded path present: %s %v", excluded, err)
		}
	}
	assertBackupPin(t, i, sha, false)
	s, err := i.BackupStats(ctx)
	if err != nil || s.Complete != 1 || s.Incomplete != 0 || s.Pins != 0 || s.LastComplete.IsZero() || s.LastVerified.IsZero() {
		t.Fatalf("stats=%+v %v", s, err)
	}
}

func TestBackupDurableFailuresResumeWithoutReleasingPinsEarly(t *testing.T) {
	for _, boundary := range []string{"pin_persisted", "common_point_captured", "blob_copied", "complete_marked"} {
		t.Run(boundary, func(t *testing.T) {
			i := backupInstance(t)
			sha, p := backupBlob(t, i, []byte("blob copied at durable boundary"))
			dest := filepath.Join(t.TempDir(), "archive")
			m, err := i.Backup(t.Context(), BackupOptions{Destination: dest, KeyID: ids.New(), fault: func(s string) error {
				if s == boundary {
					return errBackupCrash
				}
				return nil
			}})
			if !errors.Is(err, errBackupCrash) {
				t.Fatalf("fault not reached: %v", err)
			}
			assertBackupPin(t, i, sha, true)
			if boundary != "complete_marked" {
				if _, err := VerifyBackup(t.Context(), dest); err == nil {
					t.Fatal("incomplete backup verified")
				}
			}
			if boundary == "blob_copied" {
				if err := os.Remove(filepath.Join(i.Layout().Home, filepath.FromSlash(p))); err != nil {
					t.Fatal(err)
				}
			}
			home := i.Layout().Home
			if err := i.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			i = open(t, home, Options{})
			if err := i.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			m, err = i.Backup(t.Context(), BackupOptions{BackupID: m.BackupID, Destination: dest})
			if err != nil || m.State != "complete" {
				t.Fatalf("restart resume: %+v %v", m, err)
			}
			assertBackupPin(t, i, sha, false)
			if _, err := VerifyBackup(t.Context(), dest); err != nil {
				t.Fatal(err)
			}
			if boundary == "pin_persisted" {
				matches, err := filepath.Glob(dest + ".incomplete-*")
				if err != nil || len(matches) != 1 {
					t.Fatalf("interrupted capture not preserved: %v %v", matches, err)
				}
			}
		})
	}
}

func TestBackupCancelRetainsArtifactsAndDoesNotBecomeComplete(t *testing.T) {
	i := backupInstance(t)
	sha, _ := backupBlob(t, i, []byte("cancelled blob"))
	dest := filepath.Join(t.TempDir(), "archive")
	m, err := i.Backup(t.Context(), BackupOptions{Destination: dest, KeyID: ids.New(), fault: func(s string) error {
		if s == "common_point_captured" {
			return errBackupCrash
		}
		return nil
	}})
	if !errors.Is(err, errBackupCrash) {
		t.Fatal(err)
	}
	if err := i.CancelBackup(t.Context(), m.BackupID); err != nil {
		t.Fatal(err)
	}
	assertBackupPin(t, i, sha, false)
	if _, err := os.Stat(filepath.Join(dest, "manifest.json")); err != nil {
		t.Fatal("cancel removed evidence", err)
	}
	if _, err := i.Backup(t.Context(), BackupOptions{BackupID: m.BackupID}); err == nil {
		t.Fatal("cancelled backup resumed")
	}
	if _, err := VerifyBackup(t.Context(), dest); err == nil {
		t.Fatal("cancelled archive accepted")
	}
}

func completeTestBackup(t *testing.T) (*Instance, string, BackupManifest) {
	t.Helper()
	i := backupInstance(t)
	backupBlob(t, i, []byte("restore fixture blob"))
	backupWrite(t, i.Layout().Home, "projects/example/asset.yaml", []byte("revision: 1\n"))
	dest := filepath.Join(t.TempDir(), "archive")
	m, err := i.Backup(t.Context(), BackupOptions{Destination: dest, KeyID: ids.New()})
	if err != nil {
		t.Fatal(err)
	}
	return i, dest, m
}

func TestVerifyBackupRejectsTamperAndUnsafeEntries(t *testing.T) {
	for _, scenario := range []string{"checksum", "complete-marker", "missing-database", "traversal", "secret-area", "duplicate-case", "blob-layout", "symbolic-link", "manifest-link", "complete-link", "root-link"} {
		t.Run(scenario, func(t *testing.T) {
			_, dir, m := completeTestBackup(t)
			switch scenario {
			case "manifest-link", "complete-link":
				name := "manifest.json"
				if scenario == "complete-link" {
					name = "COMPLETE"
				}
				p := filepath.Join(dir, name)
				raw, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				other := filepath.Join(t.TempDir(), "same-content")
				if err := os.WriteFile(other, raw, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, p); err != nil {
					t.Skip("symlinks unavailable:", err)
				}
			case "root-link":
				link := filepath.Join(t.TempDir(), "linked-archive")
				if err := os.Symlink(dir, link); err != nil {
					t.Skip("symlinks unavailable:", err)
				}
				dir = link
			case "checksum":
				backupWrite(t, dir, "projects/example/asset.yaml", []byte("tampered"))
			case "complete-marker":
				backupWrite(t, dir, "COMPLETE", []byte("bad"))
			case "missing-database":
				for n, f := range m.Files {
					if f.Path == "db/main.db" {
						m.Files = append(m.Files[:n], m.Files[n+1:]...)
						break
					}
				}
			case "traversal":
				m.Files = append(m.Files, BackupFile{Path: "../outside", Stored: "../outside", SHA256: strings.Repeat("0", 64)})
			case "secret-area":
				m.Files = append(m.Files, BackupFile{Path: "secrets/key", Stored: "secrets/key", SHA256: strings.Repeat("0", 64)})
			case "duplicate-case":
				f := m.Files[0]
				f.Path = strings.ToUpper(f.Path)
				f.Stored = f.Path
				m.Files = append(m.Files, f)
			case "blob-layout":
				f := &m.Blobs[0]
				old := f.Path
				f.Path = "blobs/not-content-addressed"
				f.Stored = f.Path
				if err := os.Rename(filepath.Join(dir, filepath.FromSlash(old)), filepath.Join(dir, filepath.FromSlash(f.Path))); err != nil {
					t.Fatal(err)
				}
			case "symbolic-link":
				p := filepath.Join(dir, "projects/example/asset.yaml")
				raw, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				other := filepath.Join(t.TempDir(), "same-content")
				if err := os.WriteFile(other, raw, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, p); err != nil {
					t.Skip("symlinks unavailable:", err)
				}
			}
			if scenario != "checksum" && scenario != "complete-marker" && scenario != "symbolic-link" && scenario != "manifest-link" && scenario != "complete-link" && scenario != "root-link" {
				rewriteTestManifest(t, dir, m)
			}
			if _, err := VerifyBackup(t.Context(), dir); err == nil {
				t.Fatal("unsafe or corrupt archive accepted")
			}
		})
	}
}

func rewriteTestManifest(t *testing.T, dir string, m BackupManifest) {
	t.Helper()
	if err := writeBackupJSON(filepath.Join(dir, "manifest.json"), m); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	backupWrite(t, dir, "COMPLETE", []byte(string(digest.Of(raw))+"\n"))
}

func TestBackupRejectsNestedTargetAndSourceLinks(t *testing.T) {
	i := backupInstance(t)
	if _, err := i.Backup(t.Context(), BackupOptions{Destination: filepath.Join(i.Layout().Home, "backup"), KeyID: ids.New()}); err == nil {
		t.Fatal("nested backup accepted")
	}
	outside := filepath.Join(t.TempDir(), "private")
	if err := os.WriteFile(outside, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(i.Layout().Home, "projects"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(i.Layout().Home, "projects", "link")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if _, err := i.Backup(t.Context(), BackupOptions{Destination: filepath.Join(t.TempDir(), "archive"), KeyID: ids.New()}); err == nil {
		t.Fatal("symlink source accepted")
	}
}

func TestBackupCaptureResetJournalSurvivesEachDurableBoundary(t *testing.T) {
	for _, boundary := range []string{"journal", "renamed", "manifest-recreated"} {
		t.Run(boundary, func(t *testing.T) {
			i := backupInstance(t)
			sha, _ := backupBlob(t, i, []byte("reset retained blob"))
			dest := filepath.Join(t.TempDir(), "archive")
			m, err := i.Backup(t.Context(), BackupOptions{Destination: dest, KeyID: ids.New(), fault: func(s string) error {
				if s == "pin_persisted" {
					return errBackupCrash
				}
				return nil
			}})
			if !errors.Is(err, errBackupCrash) {
				t.Fatal(err)
			}
			var rec backupRecord
			if err := readBackupJSON(i.backupRecordPath(m.BackupID), &rec); err != nil {
				t.Fatal(err)
			}
			rec.RetiredDestination = dest + ".incomplete-" + string(ids.New())
			if err := writeBackupJSON(i.backupRecordPath(m.BackupID), rec); err != nil {
				t.Fatal(err)
			}
			if boundary != "journal" {
				if err := os.Rename(dest, rec.RetiredDestination); err != nil {
					t.Fatal(err)
				}
			}
			if boundary == "manifest-recreated" {
				if err := writeBackupJSON(filepath.Join(dest, "manifest.json"), rec.Manifest); err != nil {
					t.Fatal(err)
				}
			}
			m, err = i.Backup(t.Context(), BackupOptions{BackupID: m.BackupID})
			if err != nil || m.State != "complete" {
				t.Fatalf("reset boundary %s: %+v %v", boundary, m, err)
			}
			assertBackupPin(t, i, sha, false)
			if _, err := os.Stat(filepath.Join(rec.RetiredDestination, "manifest.json")); err != nil {
				t.Fatal("partial attempt not retained", err)
			}
		})
	}
}

func TestBackupAdoptsPublishedPointWhenSourceJournalLags(t *testing.T) {
	i := backupInstance(t)
	backupBlob(t, i, []byte("frozen blob"))
	backupWrite(t, i.Layout().Home, "projects/example/asset.yaml", []byte("frozen"))
	dest := filepath.Join(t.TempDir(), "archive")
	var pinRecord backupRecord
	m, err := i.Backup(t.Context(), BackupOptions{Destination: dest, KeyID: ids.New(), fault: func(stage string) error {
		if stage == "pin_persisted" {
			entries, e := os.ReadDir(filepath.Join(i.Layout().Home, "backups", "records"))
			if e != nil {
				return e
			}
			return readBackupJSON(filepath.Join(i.Layout().Home, "backups", "records", entries[0].Name()), &pinRecord)
		}
		if stage == "common_point_captured" {
			return errBackupCrash
		}
		return nil
	}})
	if !errors.Is(err, errBackupCrash) {
		t.Fatal(err)
	}
	// Simulate death after publishing destination's frozen point, before source
	// journal advances. Preserve the exact original pin record from that instant.
	if err := writeBackupJSON(i.backupRecordPath(m.BackupID), pinRecord); err != nil {
		t.Fatal(err)
	}
	backupWrite(t, i.Layout().Home, "projects/example/asset.yaml", []byte("new mutable state"))
	if _, err := i.Backup(t.Context(), BackupOptions{BackupID: m.BackupID, Check: func(context.Context) error { return errors.New("cannot recapture a published point") }}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dest, "projects/example/asset.yaml"))
	if err != nil || string(raw) != "frozen" {
		t.Fatalf("frozen point replaced: %q %v", raw, err)
	}
}
