//go:build linux || darwin

package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/oujinhaoai/lantai/internal/backupmgmt"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/operations"
)

func TestBackupPackageExportSafetyAndRestart(t *testing.T) {
	a := openTestApp(t)
	b := testBackupControl(t, a)
	j, err := b.Start(BackupRequest{})
	if err != nil {
		t.Fatal(err)
	}
	j = waitBackup(t, b, string(j.ID))
	if j.State != "complete" || j.Package == nil {
		t.Fatal(j)
	}
	path := operations.RecoveryPackagePath(filepath.Join(b.root, string(j.ID)))
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	checkGood := func(b *BackupControl) {
		t.Helper()
		f, p, err := b.OpenPackage(t.Context(), j.ID, j.Package.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		h := sha256.New()
		n, err := io.Copy(h, f)
		if err != nil || n != p.Size || hex.EncodeToString(h.Sum(nil)) != j.Package.SHA256 {
			t.Fatal("exported bytes differ", err)
		}
	}
	checkBad := func(b *BackupControl, reason string) {
		t.Helper()
		f, _, err := b.OpenPackage(t.Context(), j.ID, j.Package.SHA256)
		if f != nil {
			f.Close()
			t.Fatal("unsafe descriptor escaped")
		}
		var e *backupmgmt.PackageUnavailableError
		if !errors.As(err, &e) || !errors.Is(err, backupmgmt.ErrUnavailable) || e.Reason != "backup_package_"+reason {
			t.Fatalf("want %s, got %v", reason, err)
		}
	}
	checkGood(b)
	t.Run("hardlinks", func(t *testing.T) {
		link := path + ".extra"
		if err := os.Link(path, link); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(link)
		checkBad(b, "hardlinks")
	})
	t.Run("symlink", func(t *testing.T) {
		saved := path + ".saved"
		if err := os.Rename(path, saved); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Remove(path); _ = os.Rename(saved, path) }()
		if err := os.Symlink(saved, path); err != nil {
			t.Fatal(err)
		}
		checkBad(b, "file_type")
	})
	t.Run("permissions", func(t *testing.T) {
		if err := os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(path, 0600)
		checkBad(b, "permissions")
	})
	t.Run("owner", func(t *testing.T) {
		if os.Geteuid() != 0 {
			t.Skip("actual owner mutation requires root; covered by root Docker regression")
		}
		if err := os.Chown(path, 65532, -1); err != nil {
			t.Fatal(err)
		}
		defer os.Chown(path, 0, -1)
		checkBad(b, "owner")
	})
	t.Run("tampered", func(t *testing.T) {
		changed := append([]byte(nil), original...)
		changed[len(changed)-1] ^= 1
		if err := os.WriteFile(path, changed, 0600); err != nil {
			t.Fatal(err)
		}
		defer os.WriteFile(path, original, 0600)
		checkBad(b, "integrity")
	})
	for _, field := range []string{"digest", "size", "package_id", "backup_id"} {
		t.Run(field, func(t *testing.T) {
			mutated := j
			p := *j.Package
			mutated.Package = &p
			switch field {
			case "digest":
				p.SHA256 = hex.EncodeToString(make([]byte, 32))
			case "size":
				p.Size++
			case "package_id":
				p.ID = ids.New()
			case "backup_id":
				mutated.BackupID = ids.New()
			}
			b.mu.Lock()
			b.jobs[j.ID] = mutated
			b.mu.Unlock()
			defer func() { b.mu.Lock(); b.jobs[j.ID] = j; b.mu.Unlock() }()
			f, _, err := b.OpenPackage(t.Context(), j.ID, p.SHA256)
			if f != nil {
				f.Close()
				t.Fatal("wrong identity accepted")
			}
			var e *backupmgmt.PackageUnavailableError
			if !errors.As(err, &e) || e.Reason != "backup_package_identity" {
				t.Fatal(err)
			}
		})
	}
	checkGood(b)
	root, home := b.root, a.Instance.Layout().Home
	if err := os.Link(path, path+".restart-extra"); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(t.Context()); err != nil {
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
	got, err := b2.Status(j.ID)
	if err != nil || got.State != "complete" || got.Package.SHA256 != j.Package.SHA256 {
		t.Fatal("historical completion changed", got, err)
	}
	checkBad(b2, "hardlinks")
	if err := os.Remove(path + ".restart-extra"); err != nil {
		t.Fatal(err)
	}
	checkGood(b2)
}

func TestBackupDoesNotCompleteWithUnsafeInstalledPackage(t *testing.T) {
	a := openTestApp(t)
	b := testBackupControl(t, a)
	b.testBoundary = func(point string, j BackupJob) {
		if point == "job.worker.completed" {
			path := operations.RecoveryPackagePath(filepath.Join(b.root, string(j.ID)))
			if err := os.Link(path, path+".extra"); err != nil {
				t.Error(err)
			}
		}
	}
	j, err := b.Start(BackupRequest{})
	if err != nil {
		t.Fatal(err)
	}
	j = waitBackup(t, b, string(j.ID))
	if j.State != "failed" || j.Error != "backup_package_hardlinks" || j.Package != nil {
		t.Fatal("unsafe installed package advertised", j)
	}
	b.testBoundary = nil
	path := operations.RecoveryPackagePath(filepath.Join(b.root, string(j.ID)))
	if err := os.Remove(path + ".extra"); err != nil {
		t.Fatal(err)
	}
	j, err = b.Start(BackupRequest{Resume: j.ID})
	if err != nil {
		t.Fatal(err)
	}
	j = waitBackup(t, b, string(j.ID))
	if j.State != "complete" || j.Package == nil {
		t.Fatal("safe original package cannot resume", j)
	}
}
