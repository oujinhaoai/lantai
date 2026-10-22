package operations

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
)

func recoveryFixture(t *testing.T) (string, BackupManifest, []byte) {
	t.Helper()
	i := backupInstance(t)
	backupBlob(t, i, []byte("irreplaceable synthetic payload"))
	backupWrite(t, i.Layout().Home, "projects/synthetic/asset.yaml", []byte("title: synthetic\n"))
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(dir, "snapshot")
	m, err := i.Backup(t.Context(), BackupOptions{Destination: snapshot, KeyID: ids.New()})
	if err != nil {
		t.Fatal(err)
	}
	key := []byte("synthetic-key-material-only")
	file := filepath.Join(dir, "backup.lantai")
	info, err := WriteRecoveryPackage(t.Context(), snapshot, file, m.KeyID, func(w io.Writer) error { _, e := w.Write(key); return e })
	if err != nil {
		t.Fatal(err)
	}
	if info.Manifest.BackupID != m.BackupID || !info.Manifest.Sensitive {
		t.Fatal("lost package binding")
	}
	return file, m, key
}

func TestRecoveryPackageRoundTripAndPrivatePublication(t *testing.T) {
	file, m, key := recoveryFixture(t)
	info, err := VerifyRecoveryPackage(t.Context(), file)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.CheckPrivateFile(file); err != nil {
		t.Fatalf("package must be private: %v", err)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(file)
		if err != nil || st.Mode().Perm() != 0600 {
			t.Fatalf("Unix private mode: %v %v", st, err)
		}
	}
	dest := filepath.Join(filepath.Dir(file), "extracted")
	var gotKey []byte
	out, err := ExtractRecoveryPackage(t.Context(), file, dest, func(r io.Reader) error { var e error; gotKey, e = io.ReadAll(r); return e })
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotKey, key) || out.SHA256 != info.SHA256 {
		t.Fatal("round trip differs")
	}
	got, err := VerifyBackup(t.Context(), dest)
	if err != nil || got.BackupID != m.BackupID {
		t.Fatalf("snapshot %v %v", got, err)
	}
	if _, e := os.Stat(filepath.Join(dest, "recovery")); !os.IsNotExist(e) {
		t.Fatal("old key was written to snapshot")
	}
	if _, e := ExtractRecoveryPackage(t.Context(), file, dest, func(io.Reader) error { return nil }); e == nil {
		t.Fatal("overwrote existing target")
	}
}

func rewritePackage(t *testing.T, input []byte, mutate func(*RecoveryPackageManifest)) []byte {
	t.Helper()
	start := len(recoveryMagic) + 8
	n := int(binary.BigEndian.Uint64(input[len(recoveryMagic):start]))
	var m RecoveryPackageManifest
	if e := json.Unmarshal(input[start:start+n], &m); e != nil {
		t.Fatal(e)
	}
	mutate(&m)
	raw, e := json.Marshal(m)
	if e != nil {
		t.Fatal(e)
	}
	var out bytes.Buffer
	out.WriteString(recoveryMagic)
	_ = binary.Write(&out, binary.BigEndian, uint64(len(raw)))
	out.Write(raw)
	out.Write(input[start+n : len(input)-32])
	h := sha256.Sum256(out.Bytes())
	out.Write(h[:])
	return out.Bytes()
}

func TestRecoveryPackageRejectsMalformedContainersBeforeExtraction(t *testing.T) {
	file, _, _ := recoveryFixture(t)
	good, e := os.ReadFile(file)
	if e != nil {
		t.Fatal(e)
	}
	cases := map[string][]byte{
		"truncated":      good[:len(good)-1],
		"trailing":       append(append([]byte{}, good...), 0),
		"path_traversal": rewritePackage(t, good, func(m *RecoveryPackageManifest) { m.Files[0].Path = "snapshot/../escape" }),
		"duplicate":      rewritePackage(t, good, func(m *RecoveryPackageManifest) { m.Files[1].Path = m.Files[0].Path }),
		"case_collision": rewritePackage(t, good, func(m *RecoveryPackageManifest) { m.Files[1].Path = "SNAPSHOT/COMPLETE" }),
		"no_key":         rewritePackage(t, good, func(m *RecoveryPackageManifest) { m.Files[len(m.Files)-1].Path = "snapshot/key" }),
		"oversized":      rewritePackage(t, good, func(m *RecoveryPackageManifest) { m.Files[0].Size = recoverySizeLimit }),
		"wrong_format":   rewritePackage(t, good, func(m *RecoveryPackageManifest) { m.Contract = "other/v1" }),
	}
	corrupt := append([]byte{}, good...)
	corrupt[len(corrupt)-40] ^= 0xff
	cases["corrupt"] = corrupt
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			bad := filepath.Join(t.TempDir(), "bad.lantai")
			_ = os.WriteFile(bad, raw, 0600)
			dest := filepath.Join(t.TempDir(), "must-not-exist")
			called := false
			if _, e := ExtractRecoveryPackage(t.Context(), bad, dest, func(io.Reader) error { called = true; return nil }); e == nil {
				t.Fatal("accepted invalid package")
			}
			if called {
				t.Fatal("released key before validation")
			}
			if _, e := os.Stat(dest); !os.IsNotExist(e) {
				t.Fatal("invalid package modified destination")
			}
		})
	}
}

func TestRecoveryPackageFailedPublicationAndStableRetry(t *testing.T) {
	file, m, key := recoveryFixture(t)
	snapshot := filepath.Join(filepath.Dir(file), "snapshot")
	for _, at := range []int{1, 3, 999} {
		t.Run(fmt.Sprint(at), func(t *testing.T) {
			target := filepath.Join(filepath.Dir(file), fmt.Sprintf("failure-%d.lantai", at))
			calls := 0
			_, err := WriteRecoveryPackage(t.Context(), snapshot, target, m.KeyID, func(w io.Writer) error {
				if at == 999 {
					return errors.New("synthetic key export failure")
				}
				_, e := w.Write(key)
				return e
			}, func(context.Context) error {
				calls++
				if calls == at {
					return syscall.EIO
				}
				return nil
			})
			if err == nil {
				t.Fatal("fault did not interrupt publication")
			}
			if _, err = os.Stat(target); !os.IsNotExist(err) {
				t.Fatal("failed package was published")
			}
			left, _ := filepath.Glob(filepath.Join(filepath.Dir(file), ".recovery-package-*"))
			if len(left) != 0 {
				t.Fatal("failed package retained secret-bearing stage")
			}
			first, err := WriteRecoveryPackage(t.Context(), snapshot, target, m.KeyID, func(w io.Writer) error { _, e := w.Write(key); return e })
			if err != nil {
				t.Fatal(err)
			}
			second, err := WriteRecoveryPackage(t.Context(), snapshot, target, m.KeyID, func(w io.Writer) error { _, e := w.Write(key); return e })
			if err != nil || second.SHA256 != first.SHA256 || second.Manifest.PackageID != first.Manifest.PackageID {
				t.Fatal("retry changed a published artifact", err)
			}
			if _, err = WriteRecoveryPackage(t.Context(), snapshot, target, m.KeyID, func(w io.Writer) error { _, e := w.Write([]byte("different synthetic key")); return e }); err == nil {
				t.Fatal("existing package key silently changed")
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := WriteRecoveryPackage(ctx, snapshot, filepath.Join(filepath.Dir(file), "cancel.lantai"), m.KeyID, func(w io.Writer) error { _, e := w.Write(key); return e }); !errors.Is(err, context.Canceled) {
		t.Fatal("cancel ignored", err)
	}
}
