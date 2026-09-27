package operations

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
)

func TestCloseWaitsForBackupCopyAndRetainsRootLockOnTimeout(t *testing.T) {
	for _, boundary := range []string{"common_point_captured", "blob_copied"} {
		t.Run(boundary, func(t *testing.T) {
			i := backupInstance(t)
			sha, _ := backupBlob(t, i, []byte("shutdown must preserve the live copier"))
			destination := filepath.Join(t.TempDir(), "archive")
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			done := make(chan error, 1)
			go func() {
				_, err := i.Backup(t.Context(), BackupOptions{Destination: destination, KeyID: ids.New(), fault: func(stage string) error {
					if stage == boundary {
						close(entered)
						<-release
					}
					return nil
				}})
				done <- err
			}()
			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("backup ended before boundary: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("backup did not reach boundary")
			}
			if boundary == "blob_copied" && !i.Readiness().Ready {
				t.Fatal("test did not reach copy after common barrier release")
			}
			closeCtx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
			err := i.Close(closeCtx)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) || i.State() != StateStopping {
				t.Fatalf("close did not retain stopping state: %s %v", i.State(), err)
			}
			if i.DB(ownership.Main) == nil {
				t.Fatal("close timeout released database handles")
			}
			if err := i.DB(ownership.Main).PingContext(t.Context()); err != nil {
				t.Fatal("close timeout closed databases", err)
			}
			if second, err := Open(t.Context(), Options{Home: i.Layout().Home}); !HasReason(err, CodeInstanceLocked) {
				if second != nil {
					_ = second.Close(context.Background())
				}
				t.Fatalf("timed-out close released data-root lock: %v", err)
			}
			assertBackupPin(t, i, sha, true)
			unblock()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal("copier failed after shutdown timeout", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("copier did not finish")
			}
			if i.State() != StateStopping {
				t.Fatal("capture exit reopened a stopping instance")
			}
			newDestination := filepath.Join(t.TempDir(), "must-not-start")
			if _, err := i.Backup(t.Context(), BackupOptions{Destination: newDestination, KeyID: ids.New()}); err == nil {
				t.Fatal("stopping instance accepted another backup")
			}
			if _, err := os.Stat(newDestination); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("rejected backup created a destination", err)
			}
			if _, err := VerifyBackup(t.Context(), destination); err != nil {
				t.Fatal("finished archive invalid", err)
			}
			if err := i.Close(t.Context()); err != nil {
				t.Fatal("close after copy drain", err)
			}
			if i.State() != StateClosed {
				t.Fatal("not closed after drain")
			}
			second, err := Open(t.Context(), Options{Home: i.Layout().Home})
			if err != nil {
				t.Fatal("data-root lock not released after drain", err)
			}
			if err := second.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCancelBackupTimeoutCannotReleaseCopyPin(t *testing.T) {
	i := backupInstance(t)
	sha, _ := backupBlob(t, i, []byte("cancel must wait for copier"))
	destination := filepath.Join(t.TempDir(), "archive")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	done := make(chan error, 1)
	go func() {
		_, err := i.Backup(t.Context(), BackupOptions{Destination: destination, KeyID: ids.New(), fault: func(stage string) error {
			if stage == "blob_copied" {
				close(entered)
				<-release
				return errBackupCrash
			}
			return nil
		}})
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("copy ended before boundary: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("copy did not reach boundary")
	}
	var m BackupManifest
	if err := readBackupJSON(filepath.Join(destination, "manifest.json"), &m); err != nil {
		t.Fatal(err)
	}
	cancelCtx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	err := i.CancelBackup(cancelCtx, m.BackupID)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("cancel ignored its context deadline", err)
	}
	assertBackupPin(t, i, sha, true)
	if i.State() != StateReady {
		t.Fatal("cancel timeout changed service readiness")
	}
	unblock()
	select {
	case err := <-done:
		if !errors.Is(err, errBackupCrash) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("copy did not drain")
	}
	assertBackupPin(t, i, sha, true)
	if err := i.CancelBackup(t.Context(), m.BackupID); err != nil {
		t.Fatal("cancel after copier drained", err)
	}
	assertBackupPin(t, i, sha, false)
	if _, err := VerifyBackup(t.Context(), destination); err == nil {
		t.Fatal("cancelled interrupted copy reported complete")
	}
}
