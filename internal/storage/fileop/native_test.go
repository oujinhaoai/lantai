package fileop

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
)

var nativeCrossVolume = flag.String("fileop-cross-volume", "", "existing disposable directory on a different volume (opt-in)")

func TestNativeOpenHandleReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "held")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	held, err := holdNativeFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	err = (FS{}).ReplaceAtomic(path, []byte("new"), 0600)
	if runtime.GOOS == "windows" {
		if errcode.CodeOf(err) != errcode.StorageUnavailable {
			t.Fatalf("non-delete-sharing handle: %v", err)
		}
		b, err := os.ReadFile(path)
		if err != nil || string(b) != "old" {
			t.Fatalf("failed rename changed target: %q %v", b, err)
		}
		if err = held.Close(); err != nil {
			t.Fatal(err)
		}
		if err = (FS{}).ReplaceAtomic(path, []byte("new"), 0600); err != nil {
			t.Fatal(err)
		}
	} else {
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(held)
		if err != nil || string(b) != "old" {
			t.Fatalf("open inode changed: %q %v", b, err)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "new" {
		t.Fatalf("new pathname: %q %v", b, err)
	}
	info, err := fsutil.Inspect(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("native handle/rename %s filesystem=%s; complete old/new bytes verified", runtime.GOOS, info.Type)
}

func TestNativeCrossVolumeCopyAndRenameRefusal(t *testing.T) {
	if *nativeCrossVolume == "" {
		t.Skip("requires explicit disposable directory on another volume")
	}
	srcRoot := t.TempDir()
	dstRoot, err := os.MkdirTemp(*nativeCrossVolume, "lantai-fileop-cross-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dstRoot)
	src := filepath.Join(srcRoot, "blob")
	dst := filepath.Join(dstRoot, "copy")
	body := []byte("synthetic cross-volume bytes")
	if err = os.WriteFile(src, body, 0600); err != nil {
		t.Fatal(err)
	}
	if err = (FS{}).Link(src, dst); !errors.Is(err, ErrLinkUnsupported) {
		t.Fatalf("expected actual cross-volume link refusal (choose another volume): %v", err)
	}
	mode, err := (FS{}).LinkOrCopy(src, dst, sum(body), int64(len(body)))
	if err != nil || mode != ModeCopy {
		t.Fatalf("fallback: %s %v", mode, err)
	}
	sha, n, err := HashFile(dst)
	if err != nil || sha != sum(body) || n != int64(len(body)) {
		t.Fatal("copy digest differs", err)
	}
	directory := filepath.Join(srcRoot, "install")
	target := filepath.Join(dstRoot, "install")
	if err = os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err = (FS{}).Rename(directory, target); err == nil {
		t.Fatal("cross-volume installation rename unexpectedly succeeded")
	}
	if _, err = os.Stat(directory); err != nil {
		t.Fatal("refused rename lost source", err)
	}
	if _, err = os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused rename exposed destination", err)
	}
	a, err := fsutil.Inspect(srcRoot)
	if err != nil {
		t.Fatal(err)
	}
	b, err := fsutil.Inspect(dstRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("native cross-volume %s -> %s; verified copy sha256=%s; directory rename refused", a.Type, b.Type, sha)
}

var nativeFullVolume = flag.String("fileop-full-volume", "", "existing disposable volume with at most 256 MiB free (opt-in)")

func TestNativeFullVolumePreservesAtomicTarget(t *testing.T) {
	if *nativeFullVolume == "" {
		t.Skip("requires explicit isolated small disposable volume")
	}
	free, err := FreeBytes(*nativeFullVolume)
	if err != nil {
		t.Fatal(err)
	}
	if free > 256<<20 {
		t.Fatal("refusing to fill a volume with more than 256 MiB free")
	}
	dir, err := os.MkdirTemp(*nativeFullVolume, "lantai-fileop-full-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "target")
	if err = os.WriteFile(path, []byte("preserved old content"), 0600); err != nil {
		t.Fatal(err)
	}
	filler := filepath.Join(dir, "filler")
	f, err := os.Create(filler)
	if err != nil {
		t.Fatal(err)
	}
	block := make([]byte, 1<<20)
	var written int64
	for written < 512<<20 {
		n, e := f.Write(block)
		written += int64(n)
		if e != nil {
			err = e
			break
		}
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if Classify(err) != errcode.StorageFull {
		t.Fatalf("expected native ENOSPC/disk full; written=%d err=%v", written, err)
	}
	// Some filesystems reserve space for small allocations. A larger candidate
	// ensures the write reaches the confirmed full volume before publication.
	err = (FS{}).ReplaceAtomic(path, make([]byte, 8<<20), 0600)
	if errcode.CodeOf(err) != errcode.StorageFull {
		t.Fatalf("atomic write on full volume: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "preserved old content" {
		t.Fatalf("full volume changed existing target: %q %v", b, err)
	}
	if err = os.Remove(filler); err != nil {
		t.Fatal(err)
	}
	if err = (FS{}).ReplaceAtomic(path, []byte("complete retry"), 0600); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(path)
	if err != nil || string(b) != "complete retry" {
		t.Fatal("retry differs", err)
	}
	info, err := fsutil.Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("native full volume filesystem=%s filler_bytes=%d; STORAGE_FULL, old target preserved, retry complete", info.Type, written)
}
