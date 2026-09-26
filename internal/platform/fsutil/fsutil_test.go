package fsutil

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLockConflictsWithinProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lantai.lock")
	a, err := TryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	// 同一进程的第二个句柄也必须冲突：两个实例不能在一个进程里共用数据根。
	if _, err := TryLock(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock = %v, want ErrLocked", err)
	}
	h, err := ReadHolder(path)
	if err != nil || h.PID != os.Getpid() {
		t.Fatalf("holder = %+v, %v", h, err)
	}
	if err := a.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := a.Unlock(); err != nil {
		t.Fatalf("second unlock: %v", err)
	}
	b, err := TryLock(path)
	if err != nil {
		t.Fatalf("relock after unlock: %v", err)
	}
	b.Unlock()
}

const lockHelperEnv = "LANTAI_FSUTIL_LOCK_HELPER"

// TestLockHelper 在子进程中取锁后挂起，由父进程强杀。
func TestLockHelper(t *testing.T) {
	path := os.Getenv(lockHelperEnv)
	if path == "" {
		t.Skip("helper for TestLockReleasedWhenProcessDies")
	}
	if _, err := TryLock(path); err != nil {
		t.Fatal(err)
	}
	os.Stdout.WriteString("LOCK-HELPER-READY\n")
	time.Sleep(time.Minute)
}

func TestLockReleasedWhenProcessDies(t *testing.T) {
	if os.Getenv(lockHelperEnv) != "" {
		t.Skip("running as helper")
	}
	path := filepath.Join(t.TempDir(), "lantai.lock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockHelper$", "-test.v")
	cmd.Env = append(os.Environ(), lockHelperEnv+"="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "LOCK-HELPER-READY") {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			cmd.Process.Kill()
			cmd.Wait()
			t.Fatal("helper exited before locking")
		}
	case <-time.After(60 * time.Second):
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatal("helper did not lock in time")
	}
	if _, err := TryLock(path); !errors.Is(err, ErrLocked) {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("lock held by another process: TryLock = %v, want ErrLocked", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	// 进程崩溃后锁由操作系统释放，不留陈旧锁。
	var l *Lock
	deadline := time.Now().Add(10 * time.Second)
	for {
		l, err = TryLock(path)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("lock after holder died: %v", err)
	}
	l.Unlock()
}

func TestWriteFileAtomicReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "instance.json")
	if err := WriteFileAtomic(path, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "two" {
		t.Fatalf("content = %q, %v", got, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestCreateFileExclusive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	if err := CreateFileExclusive(path, []byte("k1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CreateFileExclusive(path, []byte("k2"), 0o600); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second create = %v, want ErrExist", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "k1" {
		t.Fatalf("existing file was overwritten: %q", got)
	}
	if err := CheckPrivate(path); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestCheckPrivateRejectsSharedFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows access is governed by ACLs")
	}
	path := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivate(path); !errors.Is(err, ErrInsecurePermissions) {
		t.Fatalf("CheckPrivate = %v", err)
	}
}

func TestFreeBytesAndInspect(t *testing.T) {
	dir := t.TempDir()
	n, err := FreeBytes(dir)
	if err != nil || n == 0 {
		t.Fatalf("FreeBytes = %d, %v", n, err)
	}
	info, err := Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("temp dir file system: %+v", info)
	if info.Remote {
		t.Fatalf("temporary directory reported as a network file system: %+v", info)
	}
}
