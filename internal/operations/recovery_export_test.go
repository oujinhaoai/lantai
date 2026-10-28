//go:build linux || darwin

package operations

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
)

func requirePackageReason(t *testing.T, err error, reason string) {
	t.Helper()
	var access *RecoveryPackageAccessError
	if !errors.As(err, &access) || access.Reason != "backup_package_"+reason {
		t.Fatalf("want %s, got %v", reason, err)
	}
}

// Trigger a deterministic concurrent mutation from the first read during
// content verification, after the initial file metadata check has passed.
type exportMutationContext struct {
	context.Context
	once   sync.Once
	mutate func()
}

func (c *exportMutationContext) Err() error {
	c.once.Do(c.mutate)
	return c.Context.Err()
}

func TestRecoveryExportRechecksMetadataAndPathAfterReading(t *testing.T) {
	for _, mutation := range []string{"hardlinks", "permissions", "replacement"} {
		t.Run(mutation, func(t *testing.T) {
			file, _, _ := recoveryFixture(t)
			ctx := &exportMutationContext{Context: t.Context(), mutate: func() {
				var err error
				switch mutation {
				case "hardlinks":
					err = os.Link(file, file+".extra")
				case "permissions":
					err = os.Chmod(file, 0644)
				case "replacement":
					err = os.Rename(file, file+".original")
					if err == nil {
						err = os.WriteFile(file, []byte("replacement"), 0600)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
			}}
			f, _, err := OpenExportRecoveryPackage(ctx, file)
			if f != nil {
				f.Close()
				t.Fatal("unsafe descriptor returned")
			}
			want := mutation
			if want == "replacement" {
				want = "changed"
			}
			requirePackageReason(t, err, want)
		})
	}
}

func TestRecoveryPublicationFinalSafetyAndRetry(t *testing.T) {
	for _, fault := range []string{"hardlinks", "permissions", "sync"} {
		t.Run(fault, func(t *testing.T) {
			file, m, key := recoveryFixture(t)
			snapshot := filepath.Join(filepath.Dir(file), "snapshot")
			target := filepath.Join(filepath.Dir(file), "retry.lantai")
			writeKey := func(w io.Writer) error { _, err := w.Write(key); return err }
			disk := recoveryPackageIO{publishRecoveryPackage, fsutil.SyncDir}
			if fault == "sync" {
				disk.syncDir = func(string) error { return errors.New("injected directory sync failure") }
			} else {
				disk.publish = func(temp, target string) error {
					if err := publishRecoveryPackage(temp, target); err != nil {
						return err
					}
					if fault == "hardlinks" {
						return os.Link(target, target+".extra")
					}
					return os.Chmod(target, 0644)
				}
			}
			if _, err := writeRecoveryPackage(t.Context(), snapshot, target, m.KeyID, writeKey, disk); err == nil {
				t.Fatal("unsafe or non-durable final package reported complete")
			} else if fault != "sync" {
				requirePackageReason(t, err, fault)
			}
			if fault != "sync" {
				_, err := WriteRecoveryPackage(t.Context(), snapshot, target, m.KeyID, writeKey)
				requirePackageReason(t, err, fault)
				if fault == "hardlinks" {
					if err := os.Remove(target + ".extra"); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Chmod(target, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				// Existing-file recovery must retry fsync; verification alone is
				// not sufficient to turn a previous durability failure into success.
				if _, err := writeRecoveryPackage(t.Context(), snapshot, target, m.KeyID, writeKey, disk); err == nil {
					t.Fatal("existing-file retry skipped directory sync")
				}
			}
			info, err := WriteRecoveryPackage(t.Context(), snapshot, target, m.KeyID, writeKey)
			if err != nil {
				t.Fatal(err)
			}
			f, actual, err := OpenExportRecoveryPackage(t.Context(), target)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if info.SHA256 != actual.SHA256 {
				t.Fatal("retry changed identity")
			}
			matches, _ := filepath.Glob(filepath.Join(filepath.Dir(file), ".recovery-package-*"))
			if len(matches) != 0 {
				t.Fatal("staging name retained", matches)
			}
		})
	}
}

func TestRecoveryPublishNoReplace(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "final")
	if err := os.WriteFile(target, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, symlink := range []bool{false, true} {
		name := target
		if symlink {
			name = filepath.Join(dir, "symlink")
			if err := os.Symlink(target, name); err != nil {
				t.Fatal(err)
			}
		}
		temp := filepath.Join(dir, "temp")
		if err := os.WriteFile(temp, []byte("new"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := publishRecoveryPackage(temp, name); !errors.Is(err, os.ErrExist) {
			t.Fatal("overwrote existing target", err)
		}
		got, _ := os.ReadFile(target)
		if string(got) != "original" {
			t.Fatal("original overwritten")
		}
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := publishRecoveryPackage(filepath.Join(dir, "temp"), target); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(target)
	if err != nil || recoveryExportMetadata(st) != nil {
		t.Fatal("publication must keep one private link", err)
	}
}

func TestRecoveryExistingPublicationRechecksAfterSync(t *testing.T) {
	file, m, key := recoveryFixture(t)
	disk := recoveryPackageIO{publishRecoveryPackage, func(string) error { return os.Link(file, file+".extra") }}
	_, err := writeRecoveryPackage(t.Context(), filepath.Join(filepath.Dir(file), "snapshot"), file, m.KeyID, func(w io.Writer) error { _, e := w.Write(key); return e }, disk)
	requirePackageReason(t, err, "hardlinks")
}

func TestRecoveryPublishConcurrentNoReplace(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "final")
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for n := range 8 {
		f, err := os.CreateTemp(dir, "candidate-")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write([]byte{byte(n)}); err != nil {
			t.Fatal(err)
		}
		if err = f.Close(); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(name string) { defer wg.Done(); results <- publishRecoveryPackage(name, target) }(f.Name())
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if !errors.Is(err, os.ErrExist) {
			t.Fatal(err)
		}
	}
	if wins != 1 {
		t.Fatal("exclusive publication winners", wins)
	}
	st, err := os.Stat(target)
	if err != nil || recoveryExportMetadata(st) != nil {
		t.Fatal("published file lost privacy", err)
	}
}
