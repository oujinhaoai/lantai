package fsutil

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestExclusiveShortWriteAndCallFailuresCleanTemp(t *testing.T) {
	for _, stage := range []string{"short_write", "file_sync", "after_file_sync", "after_publish", "link_unsupported"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "key")
			raw := []byte("complete synthetic key bytes")
			fault := errors.New("injected")
			h := exclusiveHooks{observe: func(s string) error {
				if stage == s {
					return fault
				}
				return nil
			}}
			if stage == "short_write" {
				h.write = func(f *os.File, b []byte) (int, error) { n, e := f.Write(b[:len(b)/2]); return n, e }
			}
			if stage == "link_unsupported" {
				h.link = func(string, string) error { return fault }
			}
			err := createFileExclusive(path, raw, 0600, h)
			if err == nil {
				t.Fatal("injected failure accepted")
			}
			if stage == "short_write" && !errors.Is(err, io.ErrShortWrite) {
				t.Fatal(err)
			}
			entries, e := os.ReadDir(dir)
			if e != nil {
				t.Fatal(e)
			}
			if stage == "after_publish" {
				got, e := os.ReadFile(path)
				if e != nil || !bytes.Equal(got, raw) {
					t.Fatal("published file incomplete", e)
				}
				if len(entries) != 1 {
					t.Fatal("temp retained", entries)
				}
			} else if len(entries) != 0 {
				t.Fatal("unpublished temp retained", entries)
			}
		})
	}
}

func TestExclusiveRacingCreatorNeverOverwritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	first := []byte("racing creator")
	h := exclusiveHooks{observe: func(stage string) error {
		if stage == "after_file_sync" {
			return os.WriteFile(path, first, 0600)
		}
		return nil
	}}
	if err := createFileExclusive(path, []byte("replacement"), 0600, h); !errors.Is(err, fs.ErrExist) {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, first) {
		t.Fatal("racing creator overwritten", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatal("temp retained", entries)
	}
}

func TestExclusiveConcurrentPublishOneWinner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	var wg sync.WaitGroup
	results := make(chan error, 16)
	for n := 0; n < 16; n++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- CreateFileExclusive(path, []byte("whole file"), 0600) }()
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if !errors.Is(err, fs.ErrExist) {
			t.Fatal(err)
		}
	}
	if wins != 1 {
		t.Fatal("exclusive winners", wins)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("mode", st, err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "whole file" {
		t.Fatal(string(got))
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatal("temp retained", entries)
	}
}
