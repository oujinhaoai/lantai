package operations

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func TestRestoreInterruptedCopyKeepsEpochAndLatch(t *testing.T) {
	_, archive, backup := completeTestBackup(t)
	home := filepath.Join(t.TempDir(), "restored")
	m, err := RestoreTo(t.Context(), RestoreOptions{Home: home, BackupDir: archive, KeyID: backup.KeyID, MinimumEpoch: 41, fault: func(stage string) error {
		if stage == "file_restored" {
			return errBackupCrash
		}
		return nil
	}})
	if !errors.Is(err, errBackupCrash) || m.Restore == nil || m.Restore.Stage != "copying" || m.RecoveryEpoch != 42 {
		t.Fatalf("copy fault lost latch: %+v %v", m, err)
	}
	persisted, err := ReadMarker(Layout{Home: home})
	if err != nil || persisted.Restore == nil || persisted.Restore.RunID != m.Restore.RunID {
		t.Fatalf("latch not durable: %+v %v", persisted, err)
	}
	if i, err := Open(t.Context(), Options{Home: home}); err == nil {
		defer i.Close(context.Background())
		if err := i.Start(t.Context()); err == nil {
			t.Fatal("incomplete copy started service")
		}
	}
	m2, err := RestoreTo(t.Context(), RestoreOptions{Home: home, BackupDir: archive, KeyID: backup.KeyID, MinimumEpoch: 41})
	if err != nil || m2.RecoveryEpoch != 42 || m2.Restore == nil || m2.Restore.Stage != "credentials_required" || m2.Restore.RunID != m.Restore.RunID {
		t.Fatalf("resume changed restore identity: %+v %v", m2, err)
	}
	if _, err := os.Stat(filepath.Join(home, "db/index.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("index restored from archive: %v", err)
	}
	cfg, err := LoadConfig(Layout{Home: home})
	if err != nil || cfg.SecretsDir != filepath.Join(home, "secrets") {
		t.Fatalf("secrets path not localized: %+v %v", cfg, err)
	}
	i := open(t, home, Options{})
	if !HasReason(i.Start(t.Context()), CodeRestoreIncomplete) {
		t.Fatal("restore latch did not block service startup")
	}
	if ok, _ := i.Gate().State(); ok {
		t.Fatal("restore opened writes")
	}
	if _, _, err := i.Gate().Acquire(t.Context(), commands.Request{}); err == nil {
		t.Fatal("restore accepted business command")
	}
}

func TestRestoreReviewBoundAndDomainChecksRequired(t *testing.T) {
	_, archive, backup := completeTestBackup(t)
	home := filepath.Join(t.TempDir(), "restored")
	m, err := RestoreTo(t.Context(), RestoreOptions{Home: home, BackupDir: archive, KeyID: backup.KeyID})
	if err != nil {
		t.Fatal(err)
	}
	i := open(t, home, Options{})
	if err := i.SetRestoreStage(t.Context(), "reconciliation_required"); err == nil {
		t.Fatal("stage advanced without maintenance authority")
	}
	review := RecoveryReview{Contract: "lantai.recovery-review/v1", RunID: m.Restore.RunID, BackupID: backup.BackupID, ManifestDigest: m.Restore.ManifestDigest, RecoveryEpoch: m.RecoveryEpoch, Administrator: string(ids.New()), RevocationsReconciled: true, DeletionsReconciled: true, OpenOperationsReconciled: true, EvidenceDigest: digest.Of([]byte("synthetic reconciliation evidence")), Note: "Reviewed synthetic recovery evidence."}
	checks := 0
	if err := i.OfflineMaintenance(t.Context(), func(c context.Context) error {
		if err := i.SetRestoreStage(c, "reconciliation_required"); err != nil {
			return err
		}
		wrong := review
		wrong.RunID = ids.New()
		if err := i.CompleteRestore(c, wrong, func(context.Context) error { checks++; return nil }); err == nil {
			t.Fatal("foreign review accepted")
		}
		if checks != 0 {
			t.Fatal("domain work started for foreign review")
		}
		if err := i.CompleteRestore(c, review, nil); err == nil {
			t.Fatal("missing domain checker accepted")
		}
		if err := i.CompleteRestore(c, review, func(context.Context) error { return errBackupCrash }); !errors.Is(err, errBackupCrash) {
			t.Fatal("domain rejection ignored", err)
		}
		if i.Marker().Restore == nil {
			t.Fatal("latch removed despite domain rejection")
		}
		return i.CompleteRestore(c, review, func(context.Context) error { checks++; return nil })
	}); err != nil {
		t.Fatal(err)
	}
	if checks != 1 || i.Marker().Restore != nil {
		t.Fatalf("review not completed: checks=%d marker=%+v", checks, i.Marker())
	}
	if ok, _ := i.Gate().State(); ok || i.Readiness().Ready {
		t.Fatal("review completion implicitly started serving")
	}
	if _, err := os.Stat(filepath.Join(home, "logs", "restore-"+string(review.RunID)+".json")); err != nil {
		t.Fatal("reconciliation evidence missing", err)
	}
	if err := i.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	i = open(t, home, Options{})
	completedAt := i.Marker().UpdatedAt
	if err := i.OfflineMaintenance(t.Context(), func(c context.Context) error {
		// A lost final response must replay the committed receipt even after a
		// process restart; domain mutations must not run for that same request.
		if err := i.CompleteRestore(c, review, func(context.Context) error {
			return errors.New("completed restore must not run domain checks again")
		}); err != nil {
			return err
		}
		changed := review
		changed.Note += " changed"
		if err := i.CompleteRestore(c, changed, nil); err == nil {
			t.Fatal("different review reused a completed restore receipt")
		}
		changed = review
		changed.RecoveryEpoch++
		if err := i.CompleteRestore(c, changed, nil); err == nil {
			t.Fatal("completed review replayed in another epoch")
		}
		return nil
	}); err != nil {
		t.Fatal("completed restore response could not be recovered", err)
	}
	if !i.Marker().UpdatedAt.Equal(completedAt) || i.Marker().Restore != nil {
		t.Fatal("receipt replay mutated the instance marker")
	}
	if err := i.Start(t.Context()); err != nil {
		t.Fatal("completed restored instance cannot explicitly start", err)
	}
}

func TestRestoreRejectsWrongKeyExistingTargetAndRegressedEpoch(t *testing.T) {
	_, archive, backup := completeTestBackup(t)
	for _, scenario := range []string{"key", "nonempty", "source", "epoch-limit"} {
		t.Run(scenario, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "restored")
			opts := RestoreOptions{Home: home, BackupDir: archive, KeyID: backup.KeyID}
			switch scenario {
			case "key":
				opts.KeyID = ids.New()
			case "nonempty":
				backupWrite(t, home, "sentinel", []byte("preserve"))
			case "source":
				opts.Home = archive
			case "epoch-limit":
				opts.MinimumEpoch = 9007199254740991
			}
			if _, err := RestoreTo(t.Context(), opts); err == nil {
				t.Fatal("unsafe restore accepted")
			}
			if scenario == "nonempty" {
				b, err := os.ReadFile(filepath.Join(home, "sentinel"))
				if err != nil || string(b) != "preserve" {
					t.Fatal("existing root changed")
				}
			}
		})
	}
	home := filepath.Join(t.TempDir(), "resumed")
	m, err := RestoreTo(t.Context(), RestoreOptions{Home: home, BackupDir: archive, KeyID: backup.KeyID})
	if err != nil {
		t.Fatal(err)
	}
	if m.RecoveryEpoch <= backup.RecoveryEpoch {
		t.Fatal("restore epoch did not advance")
	}
	if resumed, err := RestoreTo(t.Context(), RestoreOptions{Home: home, BackupDir: archive, KeyID: backup.KeyID, MinimumEpoch: m.RecoveryEpoch + 10}); err == nil && resumed.RecoveryEpoch <= m.RecoveryEpoch+10 {
		t.Fatal("resume ignored newly known live epoch")
	}
}

func TestRestoreResumeRejectsDestinationSymlink(t *testing.T) {
	_, archive, backup := completeTestBackup(t)
	home := filepath.Join(t.TempDir(), "restored")
	_, err := RestoreTo(t.Context(), RestoreOptions{Home: home, BackupDir: archive, KeyID: backup.KeyID, fault: func(string) error { return errBackupCrash }})
	if !errors.Is(err, errBackupCrash) {
		t.Fatal(err)
	}
	external := t.TempDir()
	if err := os.Symlink(external, filepath.Join(home, "projects")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if _, err := RestoreTo(t.Context(), RestoreOptions{Home: home, BackupDir: archive, KeyID: backup.KeyID}); err == nil {
		t.Fatal("destination symlink accepted")
	}
	entries, err := os.ReadDir(external)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outside root modified: %v %v", entries, err)
	}
}
