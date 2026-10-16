package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
)

func TestAtomicWriteObserverCallFailuresPreserveWholeVersion(t *testing.T) {
	injected := syscall.EIO
	for _, stage := range []AtomicWriteStage{AtomicFileSync, AtomicDirectorySync} {
		t.Run(string(stage), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "record.json")
			old, newer := []byte(`{"version":1}`), []byte(`{"version":2}`)
			if err := WriteFileAtomic(path, old, 0600); err != nil {
				t.Fatal(err)
			}
			var seen []AtomicWriteStage
			err := WriteFileAtomicWithObserver(path, newer, 0600, func(at AtomicWriteStage) error {
				seen = append(seen, at)
				if at == stage {
					return injected
				}
				return nil
			})
			if !errors.Is(err, injected) {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want := old
			wantStages := []AtomicWriteStage{AtomicFileSync}
			if stage == AtomicDirectorySync {
				want = newer
				wantStages = []AtomicWriteStage{AtomicFileSync, AtomicAfterFileSync, AtomicDirectorySync}
			}
			if string(got) != string(want) || !reflect.DeepEqual(seen, wantStages) {
				t.Fatalf("bytes=%s stages=%v", got, seen)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 {
				t.Fatalf("temporary entry leaked: %v %v", entries, err)
			}
			// Observer failure belongs to this invocation only. The ordinary API still
			// succeeds afterward and there is no process-global observer to clear.
			if err = WriteFileAtomic(path, old, 0600); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAtomicWriteObserverAfterFileSyncIsBeforeRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.json")
	old, newer := []byte(`{"version":1}`), []byte(`{"version":2}`)
	if err := WriteFileAtomic(path, old, 0600); err != nil {
		t.Fatal(err)
	}
	checked := false
	err := WriteFileAtomicWithObserver(path, newer, 0600, func(stage AtomicWriteStage) error {
		if stage != AtomicAfterFileSync {
			return nil
		}
		checked = true
		got, err := os.ReadFile(path)
		if err != nil || string(got) != string(old) {
			t.Fatalf("target changed before rename: %s %v", got, err)
		}
		matches, err := filepath.Glob(filepath.Join(dir, ".record.json.tmp-*"))
		if err != nil || len(matches) != 1 {
			t.Fatal(matches, err)
		}
		got, err = os.ReadFile(matches[0])
		if err != nil || string(got) != string(newer) {
			t.Fatalf("temporary record incomplete: %s %v", got, err)
		}
		return nil
	})
	if err != nil || !checked {
		t.Fatal(err, checked)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(newer) {
		t.Fatal(string(got), err)
	}
}
