//go:build windows

package extensions_test

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/extensions"
	"github.com/oujinhaoai/lantai/internal/extensions/exttest"
)

// Holding a verified executable without FILE_SHARE_DELETE reproduces the
// native deletion error deterministically; it does not depend on antivirus.
func lockHostEntry(t *testing.T, base string) func() {
	t.Helper()
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) != 1 {
		t.Fatalf("private run directory: %v %v", entries, err)
	}
	path := filepath.Join(base, entries[0].Name(), "pkg", filepath.FromSlash(exttest.EntryPath()))
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(func() { _ = windows.CloseHandle(handle) }) }
	t.Cleanup(release)
	return release
}

func TestOneShotWindowsCleanupWaitsForReleasedEntry(t *testing.T) {
	h := newHostCase(t, exttest.Spec{Server: true})
	base := t.TempDir()
	var released chan struct{}
	out, err := extensions.RunOneShot(t.Context(), extensions.RunSpec{Package: h.pkg, Files: os.DirFS(h.dir), Target: "server", Job: h.job(t, map[string]any{"mode": "unsupported"}), BaseDir: base, Deadline: time.Now().Add(20 * time.Second), ValidateResult: func([]byte) error {
		release := lockHostEntry(t, base)
		released = make(chan struct{})
		time.AfterFunc(150*time.Millisecond, func() { release(); close(released) })
		return nil
	}})
	if err != nil || out.Failure != "" || execution.ClassifyInvocation(out.Observation) != execution.InvocationCompleted || len(out.Result) == 0 {
		t.Fatal(out, err)
	}
	select {
	case <-released:
	default:
		t.Fatal("host returned before executable handle was released")
	}
	if entries, err := os.ReadDir(base); err != nil || len(entries) != 0 {
		t.Fatal("private executable residue", entries, err)
	}
}

func TestOneShotWindowsCleanupFailureRejectsResult(t *testing.T) {
	h := newHostCase(t, exttest.Spec{Server: true})
	base := t.TempDir()
	var release func()
	started := time.Now()
	out, err := extensions.RunOneShot(t.Context(), extensions.RunSpec{Package: h.pkg, Files: os.DirFS(h.dir), Target: "server", Job: h.job(t, map[string]any{"mode": "unsupported"}), BaseDir: base, Deadline: time.Now().Add(20 * time.Second), ValidateResult: func([]byte) error {
		release = lockHostEntry(t, base)
		return nil
	}})
	if release != nil {
		release()
	}
	if err != nil || out.Failure != "workdir_cleanup_failed" || len(out.Result) != 0 || len(out.Files) != 0 || !out.Observation.StopConfirmed || execution.ClassifyInvocation(out.Observation) != execution.InvocationRuntimeFault {
		t.Fatal("cleanup failure accepted a result", out, err)
	}
	if time.Since(started) > 10*time.Second {
		t.Fatal("cleanup was not bounded")
	}
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) != 1 {
		t.Fatal("locked entry was not exercised", entries, err)
	}
	if err = os.RemoveAll(filepath.Join(base, entries[0].Name())); err != nil {
		t.Fatal("released test fixture cleanup", err)
	}
}
