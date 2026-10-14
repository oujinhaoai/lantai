//go:build linux || darwin

package application

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/operations"
)

func TestBackupJournalAtomicTempsAreQuarantinedNotAdopted(t *testing.T) {
	for _, formal := range []bool{false, true} {
		for _, contents := range []string{"truncated", "complete"} {
			name := contents
			if formal {
				name += "-with-formal"
			} else {
				name += "-without-formal"
			}
			t.Run(name, func(t *testing.T) {
				a := openTestApp(t)
				b := testBackupControl(t, a)
				id := ids.New()
				now := time.Now().UTC()
				old := BackupJob{ID: id, State: "failed", Error: "backup_failed", CreatedAt: now, UpdatedAt: now}
				if formal {
					if err := b.save(old); err != nil {
						t.Fatal(err)
					}
				} else if err := os.MkdirAll(b.journalDir(), 0700); err != nil {
					t.Fatal(err)
				}
				raw := []byte(`{"id":"unfinished`)
				if contents == "complete" {
					next := old
					next.State = "complete"
					raw, _ = json.Marshal(next)
				}
				tmp, err := os.CreateTemp(b.journalDir(), "."+string(id)+".json.tmp-*")
				if err != nil {
					t.Fatal(err)
				}
				tempPath := tmp.Name()
				if _, err = tmp.Write(raw); err != nil {
					t.Fatal(err)
				}
				if err = tmp.Close(); err != nil {
					t.Fatal(err)
				}
				root, home := b.root, a.Instance.Layout().Home
				if err = a.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
				a2, err := Open(t.Context(), Options{Instance: operations.Options{Home: home}, Identity: identity.Config{Password: testPassword}})
				if err != nil {
					t.Fatal(err)
				}
				defer a2.Close(context.Background())
				b2, err := a2.EnableBackups(t.Context(), root, "synthetic-volume")
				if err != nil {
					t.Fatal(err)
				}
				got, err := b2.Status(id)
				if formal {
					if err != nil || got.State != "failed" {
						t.Fatalf("uncommitted temp replaced formal record: %+v %v", got, err)
					}
				} else if !errors.Is(err, fs.ErrNotExist) {
					t.Fatal("uncommitted job was adopted", got, err)
				}
				if _, err = os.Lstat(tempPath); !errors.Is(err, fs.ErrNotExist) {
					t.Fatal("orphan not moved", err)
				}
				quarantine := filepath.Join(home, "backups", "online-interrupted")
				entries, err := os.ReadDir(quarantine)
				if err != nil || len(entries) != 1 {
					t.Fatal(entries, err)
				}
				saved, err := os.ReadFile(filepath.Join(quarantine, entries[0].Name()))
				if err != nil || string(saved) != string(raw) {
					t.Fatal("orphan evidence changed", err)
				}
				// A second orphan with the same name cannot replace quarantined evidence.
				if err = os.WriteFile(tempPath, []byte("second orphan"), 0600); err != nil {
					t.Fatal(err)
				}
				entry, err := os.ReadDir(b2.journalDir())
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range entry {
					if e.Name() == filepath.Base(tempPath) {
						if _, err = b2.recoverJournalTemp(e); err != nil {
							t.Fatal(err)
						}
					}
				}
				entries, err = os.ReadDir(quarantine)
				if err != nil || len(entries) != 2 {
					t.Fatal("quarantine evidence overwritten", entries, err)
				}
			})
		}
	}
}

func TestBackupJournalRejectsUnknownUnsafeAndCorruptFormalEntries(t *testing.T) {
	for _, kind := range []string{"unknown", "invalid-id-temp", "noncanonical-suffix", "overflow-suffix", "temp-directory", "temp-symlink", "temp-shared", "quarantine-symlink", "quarantine-shared", "formal-directory", "formal-symlink", "formal-corrupt", "formal-mismatched-id", "formal-trailing", "formal-unknown-field", "formal-duplicate-field"} {
		t.Run(kind, func(t *testing.T) {
			a := openTestApp(t)
			b := testBackupControl(t, a)
			if err := os.MkdirAll(b.journalDir(), 0700); err != nil {
				t.Fatal(err)
			}
			id := ids.New()
			now := time.Now().UTC()
			j := BackupJob{ID: id, State: "failed", CreatedAt: now, UpdatedAt: now}
			raw, _ := json.Marshal(j)
			name := "." + string(id) + ".json.tmp-123"
			switch kind {
			case "unknown":
				name = "notes.txt"
			case "invalid-id-temp":
				name = ".not-an-id.json.tmp-123"
			case "noncanonical-suffix":
				name = "." + string(id) + ".json.tmp-001"
			case "overflow-suffix":
				name = "." + string(id) + ".json.tmp-4294967296"
			}
			if strings.HasPrefix(kind, "formal-") {
				name = string(id) + ".json"
			}
			path := filepath.Join(b.journalDir(), name)
			switch kind {
			case "temp-directory", "formal-directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "temp-symlink", "formal-symlink":
				outside := filepath.Join(t.TempDir(), "outside")
				if err := os.WriteFile(outside, raw, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			default:
				switch kind {
				case "formal-corrupt":
					raw = []byte(`{`)
				case "formal-mismatched-id":
					j.ID = ids.New()
					raw, _ = json.Marshal(j)
				case "formal-trailing":
					raw = append(raw, []byte(` {}`)...)
				case "formal-unknown-field":
					raw = append(raw[:len(raw)-1], []byte(`,"surprise":true}`)...)
				case "formal-duplicate-field":
					raw = append(raw[:len(raw)-1], []byte(`,"state":"complete"}`)...)
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				if kind == "temp-shared" {
					if err := os.Chmod(path, 0666); err != nil {
						t.Fatal(err)
					}
				}
			}
			if kind == "formal-corrupt" {
				good, _ := json.Marshal(j)
				if err := os.WriteFile(filepath.Join(b.journalDir(), "."+string(id)+".json.tmp-123"), good, 0600); err != nil {
					t.Fatal(err)
				}
			}
			quarantine := filepath.Join(a.Instance.Layout().Home, "backups", "online-interrupted")
			if kind == "quarantine-symlink" {
				if err := os.Symlink(t.TempDir(), quarantine); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "quarantine-shared" {
				if err := os.Mkdir(quarantine, 0777); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(quarantine, 0777); err != nil {
					t.Fatal(err)
				}
			}
			a.backupControl = nil
			if _, err := a.EnableBackups(t.Context(), b.root, "synthetic-volume"); err == nil {
				t.Fatal("unsafe journal entry accepted", kind)
			}
		})
	}
}

func failedCaptureForRecovery(t *testing.T) (*App, *BackupControl, BackupJob, map[string]any, string) {
	t.Helper()
	a := openTestApp(t)
	b := testBackupControl(t, a)
	realRun := b.run
	b.run = func(c context.Context, o operations.BackupOptions) (operations.BackupManifest, error) {
		o.CheckDestination = func(context.Context) error { return errors.New("stop before capture") }
		return realRun(c, o)
	}
	j, err := b.Start(BackupRequest{})
	if err != nil {
		t.Fatal(err)
	}
	j = waitBackup(t, b, string(j.ID))
	if j.State != "failed" || !j.BackupID.Valid() {
		t.Fatal(j)
	}
	path := filepath.Join(a.Instance.Layout().Home, "backups", "records", string(j.BackupID)+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err = json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	return a, b, j, rec, path
}
func writeRecoveryJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestBackupResumeAllCaptureResetBoundaries(t *testing.T) {
	for _, stage := range []string{"journal", "rename", "empty", "manifest", "cleared"} {
		for _, missingID := range []bool{false, true} {
			label := stage
			if missingID {
				label += "-unknown-id"
			}
			t.Run(label, func(t *testing.T) {
				a, b, j, rec, path := failedCaptureForRecovery(t)
				target := filepath.Join(b.root, string(j.ID))
				oldID := j.BackupID
				retired := target + ".incomplete-" + string(ids.New())
				rec["retired_destination"] = retired
				writeRecoveryJSON(t, path, rec)
				oldManifest, err := os.ReadFile(filepath.Join(target, "manifest.json"))
				if err != nil {
					t.Fatal(err)
				}
				if stage != "journal" {
					if err = os.Rename(target, retired); err != nil {
						t.Fatal(err)
					}
				}
				if stage == "empty" || stage == "manifest" || stage == "cleared" {
					if err = os.Mkdir(target, 0700); err != nil {
						t.Fatal(err)
					}
				}
				if stage == "manifest" || stage == "cleared" {
					if err = os.WriteFile(filepath.Join(target, "manifest.json"), oldManifest, 0600); err != nil {
						t.Fatal(err)
					}
				}
				if stage == "cleared" {
					delete(rec, "retired_destination")
					writeRecoveryJSON(t, path, rec)
				}
				j.State = "backing_up"
				if missingID {
					j.BackupID = ""
				}
				if err = b.save(j); err != nil {
					t.Fatal(err)
				}
				root, home := b.root, a.Instance.Layout().Home
				if err = a.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
				a2, err := Open(t.Context(), Options{Instance: operations.Options{Home: home}, Identity: identity.Config{Password: testPassword}})
				if err != nil {
					t.Fatal(err)
				}
				defer a2.Close(context.Background())
				b2, err := a2.EnableBackups(t.Context(), root, "synthetic-volume")
				if err != nil {
					t.Fatal(err)
				}
				if _, err = b2.Start(BackupRequest{Resume: j.ID}); err != nil {
					t.Fatal(err)
				}
				got := waitBackup(t, b2, string(j.ID))
				if got.State != "complete" || got.BackupID != oldID {
					t.Fatalf("%s: %+v", label, got)
				}
				if _, err = operations.VerifyBackup(t.Context(), target); err != nil {
					t.Fatal(err)
				}
				kept, err := os.ReadFile(filepath.Join(retired, "manifest.json"))
				if err != nil || string(kept) != string(oldManifest) {
					t.Fatal("retired capture was replaced", err)
				}
			})
		}
	}
}

func TestBackupResumeRejectsUnboundOrSubstitutedResetState(t *testing.T) {
	for _, kind := range []string{"missing-without-reset", "copying-missing", "retired-foreign-id", "retired-foreign-key", "retired-copying", "retired-symlink", "source-foreign-instance", "source-foreign-id", "source-foreign-destination", "unknown-id-no-source", "unknown-id-ambiguous-source", "rebuilt-foreign-key"} {
		t.Run(kind, func(t *testing.T) {
			a, b, j, rec, path := failedCaptureForRecovery(t)
			target := filepath.Join(b.root, string(j.ID))
			retired := target + ".incomplete-" + string(ids.New())
			if err := os.Rename(target, retired); err != nil {
				t.Fatal(err)
			}
			rec["retired_destination"] = retired
			manifest := rec["manifest"].(map[string]any)
			var retiredManifest map[string]any
			raw, err := os.ReadFile(filepath.Join(retired, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(raw, &retiredManifest); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "missing-without-reset":
				delete(rec, "retired_destination")
			case "copying-missing":
				delete(rec, "retired_destination")
				manifest["state"] = "copying"
			case "retired-foreign-id":
				retiredManifest["backup_id"] = string(ids.New())
			case "retired-foreign-key":
				retiredManifest["key_id"] = string(ids.New())
			case "retired-copying":
				retiredManifest["state"] = "copying"
			case "source-foreign-instance":
				manifest["instance_id"] = string(ids.New())
			case "source-foreign-id":
				manifest["backup_id"] = string(ids.New())
			case "source-foreign-destination":
				rec["destination"] = filepath.Join(t.TempDir(), "different")
			case "unknown-id-no-source":
				j.BackupID = ""
			case "unknown-id-ambiguous-source":
				duplicate := ids.New()
				manifest["backup_id"] = string(duplicate)
				writeRecoveryJSON(t, filepath.Join(filepath.Dir(path), string(duplicate)+".json"), rec)
				manifest["backup_id"] = string(j.BackupID)
				j.BackupID = ""
			}
			writeRecoveryJSON(t, path, rec)
			writeRecoveryJSON(t, filepath.Join(retired, "manifest.json"), retiredManifest)
			if kind == "unknown-id-no-source" {
				if err = os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "retired-symlink" {
				moved := retired + "-outside"
				if err = os.Rename(retired, moved); err != nil {
					t.Fatal(err)
				}
				if err = os.Symlink(moved, retired); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "rebuilt-foreign-key" {
				if err = os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
				retiredManifest["key_id"] = string(ids.New())
				writeRecoveryJSON(t, filepath.Join(target, "manifest.json"), retiredManifest)
			}
			b.run = a.Backup
			if err = b.save(j); err != nil {
				t.Fatal(err)
			}
			b.mu.Lock()
			b.jobs[j.ID] = j
			b.mu.Unlock()
			if _, err = b.Start(BackupRequest{Resume: j.ID}); err != nil {
				t.Fatal(err)
			}
			got := waitBackup(t, b, string(j.ID))
			if got.State != "failed" {
				t.Fatal("unsafe reset accepted", kind, got)
			}
			if kind == "copying-missing" {
				if _, e := os.Stat(target); !errors.Is(e, fs.ErrNotExist) {
					t.Fatal("missing frozen point was recreated", e)
				}
			}
			if _, err = operations.VerifyBackup(t.Context(), target); err == nil {
				t.Fatal("unsafe state published complete", kind)
			}
		})
	}
}
