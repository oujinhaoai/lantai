package application

import (
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"testing"
)

func TestNativeReturnRequiresActualChangedBytes(t *testing.T) {
	base := []manifest.File{{Path: "main.blend", SHA256: "old", Size: 3, Role: "primary"}, {Path: "notes.txt", SHA256: "note", Size: 4, Role: "source"}}
	for _, files := range [][]manifest.File{
		{{Path: "renamed.blend", SHA256: "old", Size: 3}, {Path: "notes.txt", SHA256: "note", Size: 4}},
		{{Path: "notes.txt", SHA256: "note", Size: 4}},
		{{Path: "main.blend", SHA256: "old", Size: 3, Role: "source"}, {Path: "notes.txt", SHA256: "note", Size: 4, Role: "primary"}},
	} {
		if nativeContentChanged(base, files) {
			t.Fatal("rename, deletion or role change was accepted as edited bytes")
		}
	}
	if !nativeContentChanged(base, []manifest.File{{Path: "main.blend", SHA256: "changed", Size: 3}, {Path: "notes.txt", SHA256: "note", Size: 4}}) {
		t.Fatal("actual same-path changed bytes not recognized")
	}
}
