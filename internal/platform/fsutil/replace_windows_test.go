//go:build windows

package fsutil

import (
	"bytes"
	"errors"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func holdAtomicTarget(t *testing.T, path string) func() {
	t.Helper()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(func() { windows.CloseHandle(handle) }) }
	t.Cleanup(release)
	return release
}

func TestWriteFileAtomicWindowsWaitsForOccupiedTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	if err := WriteFileAtomic(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	release := holdAtomicTarget(t, path)
	done := make(chan error, 1)
	go func() { done <- WriteFileAtomic(path, []byte("new"), 0600) }()
	// A held native handle is deterministic; check the writer is still waiting
	// and readers retain the complete old file before releasing it.
	select {
	case err := <-done:
		t.Fatalf("replacement completed with target held: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	before, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, []byte("old")) {
		t.Fatal("occupied target changed", err)
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("released replacement did not finish")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, []byte("new")) {
		t.Fatal("replacement bytes", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatal("temporary residue", entries, err)
	}
}

func TestWriteFileAtomicWindowsPersistentDenialPreservesTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	if err := WriteFileAtomic(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	release := holdAtomicTarget(t, path)
	start := time.Now()
	err := WriteFileAtomic(path, []byte("new"), 0600)
	if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatal("persistent denial did not propagate", err)
	}
	if elapsed := time.Since(start); elapsed < 2*time.Second || elapsed > 10*time.Second {
		t.Fatal("replacement retry was not bounded", elapsed)
	}
	old, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(old, []byte("old")) {
		t.Fatal("failed replacement changed target", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatal("failed replacement left temporary file", entries, err)
	}
	release()
	if err = WriteFileAtomic(path, []byte("retry"), 0600); err != nil {
		t.Fatal(err)
	}
}
