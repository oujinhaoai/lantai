package fileop

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestClassify(t *testing.T) {
	cases := map[error]errcode.Code{
		&fs.PathError{Op: "write", Path: "x", Err: syscall.ENOSPC}:  errcode.StorageFull,
		&fs.PathError{Op: "rename", Path: "x", Err: syscall.EBUSY}:  errcode.StorageUnavailable,
		&fs.PathError{Op: "open", Path: "x", Err: fs.ErrPermission}: errcode.StorageUnavailable,
		errors.New("something else"):                                "",
		errcode.New(errcode.HashMismatch, ""):                       errcode.HashMismatch,
	}
	for err, want := range cases {
		if got := Classify(err); got != want {
			t.Errorf("Classify(%v) = %q, want %q", err, got, want)
		}
	}
	if w := Wrap("writing", &fs.PathError{Op: "write", Path: "/secret/host/path", Err: syscall.ENOSPC}); errcode.CodeOf(w) != errcode.StorageFull {
		t.Fatalf("Wrap = %v", w)
	} else if e, _ := errcode.As(w); e.Message != "storage is full while writing" {
		t.Fatalf("message leaks details: %q", e.Message)
	}
}

func TestCreateOrMatch(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a", "b", "record.json")
	var f FS
	created, err := f.CreateOrMatch(p, []byte("one"), 0o444)
	if err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	created, err = f.CreateOrMatch(p, []byte("one"), 0o444)
	if err != nil || created {
		t.Fatalf("same content must be idempotent: %v %v", created, err)
	}
	if _, err := f.CreateOrMatch(p, []byte("two"), 0o444); !errors.Is(err, ErrContentDiffers) {
		t.Fatalf("different content: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "one" {
		t.Fatalf("existing content was overwritten: %q", b)
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, "a", "b", ".*tmp*"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}
}

func TestCreateOrMatchWithoutHardlinks(t *testing.T) {
	dir := t.TempDir()
	f := FS{Faults: &Faults{Link: func(string, string) error { return syscall.EXDEV }}}
	p := filepath.Join(dir, "x.yaml")
	if created, err := f.CreateOrMatch(p, []byte("v"), 0o644); err != nil || !created {
		t.Fatalf("fallback create: %v %v", created, err)
	}
	if created, err := f.CreateOrMatch(p, []byte("v"), 0o644); err != nil || created {
		t.Fatalf("fallback replay: %v %v", created, err)
	}
	if _, err := f.CreateOrMatch(p, []byte("w"), 0o644); !errors.Is(err, ErrContentDiffers) {
		t.Fatalf("fallback conflict: %v", err)
	}
}

func TestWriteFaultsAreClassified(t *testing.T) {
	dir := t.TempDir()
	full := FS{Faults: &Faults{Write: func(string) error { return syscall.ENOSPC }}}
	if _, err := full.CreateOrMatch(filepath.Join(dir, "a"), []byte("x"), 0o644); errcode.CodeOf(err) != errcode.StorageFull {
		t.Fatalf("disk full: %v", err)
	}
	if err := full.ReplaceAtomic(filepath.Join(dir, "b"), []byte("x"), 0o644); errcode.CodeOf(err) != errcode.StorageFull {
		t.Fatalf("disk full on replace: %v", err)
	}
	busy := FS{Faults: &Faults{Rename: func(string, string) error { return syscall.EBUSY }}}
	if err := busy.ReplaceAtomic(filepath.Join(dir, "c"), []byte("x"), 0o644); errcode.CodeOf(err) != errcode.StorageUnavailable {
		t.Fatalf("file in use: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a failed write left a file behind")
	}
}

func TestReplaceAtomic(t *testing.T) {
	p := filepath.Join(t.TempDir(), "asset.yaml")
	var f FS
	for _, v := range []string{"first", "second"} {
		if err := f.ReplaceAtomic(p, []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(p); string(b) != v {
			t.Fatalf("content = %q", b)
		}
	}
}

func TestLinkOrCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "blob")
	data := []byte("immutable content")
	if err := os.WriteFile(src, data, 0o444); err != nil {
		t.Fatal(err)
	}
	var f FS
	mode, err := f.LinkOrCopy(src, filepath.Join(dir, "linked"), sum(data), int64(len(data)))
	if err != nil || mode != ModeHardlink {
		t.Fatalf("link: %v %v", mode, err)
	}
	a, _ := os.Stat(src)
	b, _ := os.Stat(filepath.Join(dir, "linked"))
	if !os.SameFile(a, b) {
		t.Fatal("hard link does not share the file")
	}
	if _, err := f.LinkOrCopy(src, filepath.Join(dir, "linked"), sum(data), int64(len(data))); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("existing destination: %v", err)
	}

	noLinks := FS{Faults: &Faults{Link: func(string, string) error { return syscall.EXDEV }}}
	mode, err = noLinks.LinkOrCopy(src, filepath.Join(dir, "copied"), sum(data), int64(len(data)))
	if err != nil || mode != ModeCopy {
		t.Fatalf("copy fallback: %v %v", mode, err)
	}
	c, _ := os.Stat(filepath.Join(dir, "copied"))
	if os.SameFile(a, c) {
		t.Fatal("fallback should copy, not link")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "copied")); string(got) != string(data) {
		t.Fatal("copy differs")
	}
	if _, err := noLinks.LinkOrCopy(src, filepath.Join(dir, "bad"), sum([]byte("other")), int64(len(data))); errcode.CodeOf(err) != errcode.HashMismatch {
		t.Fatalf("copy with a wrong expected hash: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "bad")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a copy that failed verification was left in place")
	}
	failing := FS{Faults: &Faults{Link: func(string, string) error { return syscall.ENOSPC }}}
	if _, err := failing.LinkOrCopy(src, filepath.Join(dir, "full"), sum(data), int64(len(data))); errcode.CodeOf(err) != errcode.StorageFull {
		t.Fatalf("link on a full disk: %v", err)
	}
}

func TestHashFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	data := []byte("hash me")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	h, n, err := HashFile(p)
	if err != nil || h != sum(data) || n != int64(len(data)) {
		t.Fatalf("HashFile = %s %d %v", h, n, err)
	}
}

func TestCreateOrMatchRetriesDirectorySync(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "record.json")
	failed := true
	f := FS{Faults: &Faults{Sync: func(path string) error {
		if failed && path == dir {
			return syscall.EIO
		}
		return nil
	}}}
	for i := 0; i < 2; i++ {
		if _, err := f.CreateOrMatch(p, []byte("durable record"), 0o444); errcode.CodeOf(err) != errcode.StorageUnavailable {
			t.Fatalf("attempt %d bypassed a failed directory sync: %v", i, err)
		}
	}
	failed = false
	if created, err := f.CreateOrMatch(p, []byte("durable record"), 0o444); err != nil || created {
		t.Fatalf("recovery after sync resumes: created=%v err=%v", created, err)
	}
}

func TestConcurrentCreateWithoutHardlinksNeverOverwrites(t *testing.T) {
	p := filepath.Join(t.TempDir(), "record.json")
	f := FS{Faults: &Faults{Link: func(string, string) error { return syscall.EXDEV }}}
	const writers = 32
	start := make(chan struct{})
	type result struct {
		data    string
		created bool
		err     error
	}
	results := make(chan result, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			<-start
			data := "record-" + strconv.Itoa(i)
			created, err := f.CreateOrMatch(p, []byte(data), 0o444)
			results <- result{data, created, err}
		})
	}
	close(start)
	wg.Wait()
	close(results)
	var winner string
	for r := range results {
		if r.created && r.err == nil {
			if winner != "" {
				t.Fatalf("more than one writer published: %s and %s", winner, r.data)
			}
			winner = r.data
		} else if !errors.Is(r.err, ErrContentDiffers) {
			t.Fatalf("conflicting writer: created=%v err=%v", r.created, r.err)
		}
	}
	if raw, err := os.ReadFile(p); err != nil || winner == "" || string(raw) != winner {
		t.Fatalf("immutable result = %q, winner=%q err=%v", raw, winner, err)
	}
}
