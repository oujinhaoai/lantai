//go:build linux || darwin

package application

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/operations"
)

func TestOnlineBackupResumeRejectsReplacedPointWithoutRebinding(t *testing.T) {
	for _, state := range []string{"copying", "complete"} {
		for _, known := range []bool{false, true} {
			for _, foreign := range []bool{false, true} {
				name := state
				if known {
					name += "/known_id"
				} else {
					name += "/missing_id"
				}
				if foreign {
					name += "/foreign_instance"
				} else {
					name += "/same_instance"
				}
				t.Run(name, func(t *testing.T) {
					a := openTestApp(t)
					b := testBackupControl(t, a)
					first, err := b.Start(BackupRequest{})
					if err != nil {
						t.Fatal(err)
					}
					first = waitBackup(t, b, string(first.ID))
					if first.State != "complete" {
						t.Fatal(first)
					}
					other := b
					if foreign {
						other = testBackupControl(t, openTestApp(t))
					}
					second, err := other.Start(BackupRequest{})
					if err != nil {
						t.Fatal(err)
					}
					second = waitBackup(t, other, string(second.ID))
					if second.State != "complete" {
						t.Fatal(second)
					}
					expected := first.BackupID
					recordPath := filepath.Join(a.Instance.Layout().Home, "backups", "records", string(expected)+".json")
					if state == "copying" {
						raw, e := os.ReadFile(recordPath)
						if e != nil {
							t.Fatal(e)
						}
						var r map[string]json.RawMessage
						if e = json.Unmarshal(raw, &r); e != nil {
							t.Fatal(e)
						}
						var manifest map[string]any
						if e = json.Unmarshal(r["manifest"], &manifest); e != nil {
							t.Fatal(e)
						}
						manifest["state"] = "copying"
						delete(manifest, "completed_at")
						r["manifest"], e = json.Marshal(manifest)
						if e != nil {
							t.Fatal(e)
						}
						raw, e = json.Marshal(r)
						if e != nil {
							t.Fatal(e)
						}
						if e = os.WriteFile(recordPath, raw, 0600); e != nil {
							t.Fatal(e)
						}
					}
					recordBefore, _ := os.ReadFile(recordPath)
					first.State = "backing_up"
					if !known {
						first.BackupID = ""
					}
					if err = b.save(first); err != nil {
						t.Fatal(err)
					}
					root, home := b.root, a.Instance.Layout().Home
					target := filepath.Join(root, string(first.ID))
					if err = a.Close(t.Context()); err != nil {
						t.Fatal(err)
					}
					if err = os.Rename(target, target+".preserved"); err != nil {
						t.Fatal(err)
					}
					if err = os.Rename(filepath.Join(other.root, string(second.ID)), target); err != nil {
						t.Fatal(err)
					}
					replacementBefore, _ := os.ReadFile(filepath.Join(target, "manifest.json"))
					a2, err := Open(t.Context(), Options{Instance: operations.Options{Home: home}, Identity: identity.Config{Password: testPassword}})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = a2.Close(context.Background()) })
					b2, err := a2.EnableBackups(t.Context(), root, "synthetic-volume")
					if err != nil {
						t.Fatal(err)
					}
					if _, err = b2.Start(BackupRequest{Resume: first.ID}); err != nil {
						t.Fatal(err)
					}
					got := waitBackup(t, b2, string(first.ID))
					if got.State != "failed" || got.BackupID != expected || !a2.Ready() {
						t.Fatalf("wrong result or gate not restored: %+v ready=%v", got, a2.Ready())
					}
					recordAfter, _ := os.ReadFile(recordPath)
					replacementAfter, _ := os.ReadFile(filepath.Join(target, "manifest.json"))
					if string(recordBefore) != string(recordAfter) || string(replacementBefore) != string(replacementAfter) {
						t.Fatal("failed resume modified source or replacement")
					}
					if err = os.Rename(target, target+".rejected"); err != nil {
						t.Fatal(err)
					}
					if err = os.Rename(target+".preserved", target); err != nil {
						t.Fatal(err)
					}
					if _, err = b2.Start(BackupRequest{Resume: first.ID}); err != nil {
						t.Fatal(err)
					}
					got = waitBackup(t, b2, string(first.ID))
					if got.State != "complete" || got.BackupID != expected {
						t.Fatalf("original point not recovered: %+v", got)
					}
				})
			}
		}
	}
}

func TestOnlineBackupWorkerCannotRebindResolvedJob(t *testing.T) {
	for _, workerError := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[workerError], func(t *testing.T) {
			a := openTestApp(t)
			b := testBackupControl(t, a)
			j, err := b.Start(BackupRequest{})
			if err != nil {
				t.Fatal(err)
			}
			j = waitBackup(t, b, string(j.ID))
			if j.State != "complete" {
				t.Fatal(j)
			}
			expected := j.BackupID
			b.mu.Lock()
			j.State = "failed"
			b.jobs[j.ID] = j
			b.mu.Unlock()
			b.run = func(context.Context, operations.BackupOptions) (operations.BackupManifest, error) {
				var err error
				if workerError {
					err = context.Canceled
				}
				return operations.BackupManifest{BackupID: ids.New()}, err
			}
			if _, err := b.Start(BackupRequest{Resume: j.ID}); err != nil {
				t.Fatal(err)
			}
			got := waitBackup(t, b, string(j.ID))
			if got.State != "failed" || got.BackupID != expected {
				t.Fatalf("worker rebound job: %+v", got)
			}
		})
	}
}
