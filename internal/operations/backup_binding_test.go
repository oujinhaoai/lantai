package operations

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
)

func TestBackupResumeBindsEntireFrozenPoint(t *testing.T) {
	mutations := map[string]func(*BackupManifest){
		"backup_id":    func(m *BackupManifest) { m.BackupID = ids.New() },
		"instance_id":  func(m *BackupManifest) { m.InstanceID = ids.New() },
		"key_id":       func(m *BackupManifest) { m.KeyID = ids.New() },
		"epoch":        func(m *BackupManifest) { m.RecoveryEpoch++ },
		"created_at":   func(m *BackupManifest) { m.CreatedAt = m.CreatedAt.Add(time.Second) },
		"completed_at": func(m *BackupManifest) { m.CompletedAt = m.CompletedAt.Add(time.Second) },
		"matrix":       func(m *BackupManifest) { m.Matrix.Databases[ownership.Main]++ },
		"watermarks":   func(m *BackupManifest) { m.Watermarks["events"]++ },
		"files":        func(m *BackupManifest) { m.Files[0].Size++ },
		"blobs":        func(m *BackupManifest) { m.Blobs = nil },
	}
	for _, state := range []string{"copying", "complete"} {
		for name, mutate := range mutations {
			if state == "copying" && name == "completed_at" {
				continue
			} // completion is the permitted transition
			t.Run(state+"/"+name, func(t *testing.T) {
				i, dest, original := completeTestBackup(t)
				var rec backupRecord
				if err := readBackupJSON(i.backupRecordPath(original.BackupID), &rec); err != nil {
					t.Fatal(err)
				}
				rec.Manifest.State = state
				if state == "copying" {
					rec.Manifest.CompletedAt = time.Time{}
					rec.PinReleasedAt = time.Time{}
					rec.VerifiedAt = time.Time{}
				}
				if err := writeBackupJSON(i.backupRecordPath(original.BackupID), rec); err != nil {
					t.Fatal(err)
				}
				recordBefore, _ := os.ReadFile(i.backupRecordPath(original.BackupID))
				mutate(&original)
				rewriteTestManifest(t, dest, original)
				targetBefore, _ := os.ReadFile(filepath.Join(dest, "manifest.json"))
				result, err := i.Backup(t.Context(), BackupOptions{BackupID: rec.Manifest.BackupID, Destination: dest})
				if err == nil || result.BackupID != rec.Manifest.BackupID {
					t.Fatalf("rebound point: result=%+v err=%v", result, err)
				}
				recordAfter, _ := os.ReadFile(i.backupRecordPath(rec.Manifest.BackupID))
				targetAfter, _ := os.ReadFile(filepath.Join(dest, "manifest.json"))
				if string(recordBefore) != string(recordAfter) || string(targetBefore) != string(targetAfter) {
					t.Fatal("rejected replacement was modified")
				}
			})
		}
	}
}

func TestBackupCopyResumeRejectsSubstitutionWithoutCompleteMarker(t *testing.T) {
	for _, marker := range []string{"missing", "invalid"} {
		t.Run(marker, func(t *testing.T) {
			i, dest, m := completeTestBackup(t)
			var rec backupRecord
			if err := readBackupJSON(i.backupRecordPath(m.BackupID), &rec); err != nil {
				t.Fatal(err)
			}
			rec.Manifest.State, rec.Manifest.CompletedAt = "copying", time.Time{}
			if err := writeBackupJSON(i.backupRecordPath(m.BackupID), rec); err != nil {
				t.Fatal(err)
			}
			m.KeyID = ids.New() // self-consistent files, but belongs to a different frozen point
			rewriteTestManifest(t, dest, m)
			if marker == "missing" {
				if err := os.Remove(filepath.Join(dest, "COMPLETE")); err != nil {
					t.Fatal(err)
				}
			} else {
				backupWrite(t, dest, "COMPLETE", []byte("invalid\n"))
			}
			before, _ := os.ReadFile(filepath.Join(dest, "manifest.json"))
			if _, err := i.Backup(t.Context(), BackupOptions{BackupID: rec.Manifest.BackupID}); err == nil {
				t.Fatal("replacement overwritten and accepted")
			}
			after, _ := os.ReadFile(filepath.Join(dest, "manifest.json"))
			if string(before) != string(after) {
				t.Fatal("substituted target overwritten")
			}
		})
	}
}

func TestBackupCopyResumeAcceptsSamePointWithIncompleteMarker(t *testing.T) {
	for _, marker := range []string{"missing", "invalid", "valid"} {
		t.Run(marker, func(t *testing.T) {
			i, dest, m := completeTestBackup(t)
			var rec backupRecord
			if err := readBackupJSON(i.backupRecordPath(m.BackupID), &rec); err != nil {
				t.Fatal(err)
			}
			rec.Manifest.State, rec.Manifest.CompletedAt = "copying", time.Time{}
			rec.PinReleasedAt, rec.VerifiedAt = time.Time{}, time.Time{}
			if err := writeBackupJSON(i.backupRecordPath(m.BackupID), rec); err != nil {
				t.Fatal(err)
			}
			if marker == "missing" {
				if err := os.Remove(filepath.Join(dest, "COMPLETE")); err != nil {
					t.Fatal(err)
				}
			} else if marker == "invalid" {
				backupWrite(t, dest, "COMPLETE", []byte("invalid\n"))
			}
			got, err := i.Backup(t.Context(), BackupOptions{BackupID: m.BackupID, Check: func(context.Context) error { t.Fatal("recaptured published point"); return nil }})
			if err != nil || got.BackupID != m.BackupID || got.State != "complete" {
				t.Fatalf("same point not recovered: %+v %v", got, err)
			}
			if _, err := VerifyBackup(t.Context(), dest); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBackupPublishedCaptureRejectsChangedPinBinding(t *testing.T) {
	for _, field := range []string{"instance", "epoch", "key", "created", "watermark", "blobs", "format"} {
		t.Run(field, func(t *testing.T) {
			i := backupInstance(t)
			backupBlob(t, i, []byte("pinned"))
			dest := filepath.Join(t.TempDir(), "archive")
			var pinned backupRecord
			m, err := i.Backup(t.Context(), BackupOptions{Destination: dest, KeyID: ids.New(), fault: func(s string) error {
				if s == "pin_persisted" {
					entries, e := os.ReadDir(filepath.Join(i.Layout().Home, "backups", "records"))
					if e != nil {
						return e
					}
					return readBackupJSON(filepath.Join(i.Layout().Home, "backups", "records", entries[0].Name()), &pinned)
				}
				if s == "common_point_captured" {
					return errBackupCrash
				}
				return nil
			}})
			if err != errBackupCrash {
				t.Fatal(err)
			}
			if err := writeBackupJSON(i.backupRecordPath(m.BackupID), pinned); err != nil {
				t.Fatal(err)
			}
			switch field {
			case "instance":
				m.InstanceID = ids.New()
			case "epoch":
				m.RecoveryEpoch++
			case "key":
				m.KeyID = ids.New()
			case "created":
				m.CreatedAt = m.CreatedAt.Add(time.Second)
			case "watermark":
				m.Watermarks["events"] = 20
			case "blobs":
				m.Blobs = nil
			case "format":
				m.Matrix.DataFormatVersion++
			}
			if err := writeBackupJSON(filepath.Join(dest, "manifest.json"), m); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(i.backupRecordPath(m.BackupID))
			if _, err := i.Backup(t.Context(), BackupOptions{BackupID: m.BackupID}); err == nil {
				t.Fatal("adopted different pinned point")
			}
			after, _ := os.ReadFile(i.backupRecordPath(m.BackupID))
			if string(before) != string(after) {
				t.Fatal("source pin changed on rejected adoption")
			}
		})
	}
}

func TestBackupBindingUsesPersistedTimestampPrecision(t *testing.T) {
	_, _, m := completeTestBackup(t)
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var persisted BackupManifest
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if err := validateBackupPoint(m, persisted); err != nil {
		t.Fatal(err)
	}
}
