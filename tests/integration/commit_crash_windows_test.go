//go:build windows

package integration

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func crashReadyReadPending(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

func TestCommitCrashHandshakeWindowsOccupiedRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready.json")
	writeCrashJSON(t, path, crashHandshake{Home: "synthetic-owned-root"})
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = windows.CloseHandle(handle)
		}
	}()
	if _, err = os.ReadFile(path); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) || !crashReadyReadPending(err) {
		t.Fatalf("occupied read must be pending: %v", err)
	}
	if err = windows.CloseHandle(handle); err != nil {
		t.Fatal(err)
	}
	closed = true
	if _, err = os.ReadFile(path); err != nil || crashReadyReadPending(err) {
		t.Fatalf("released handshake must be readable: %v", err)
	}
	if crashReadyReadPending(&os.PathError{Op: "open", Path: path, Err: windows.ERROR_ACCESS_DENIED}) {
		t.Fatal("permission failure must not become pending")
	}
}
