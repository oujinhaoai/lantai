package client

import (
	"os"
	"path/filepath"
	"testing"
)

func pinnedOutputTestRoot(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPinnedPrivateOutputWriteReplaceAndLimits(t *testing.T) {
	root := pinnedOutputTestRoot(t)
	path := filepath.Join(root, "new", "nested", "report.json")
	o, err := OpenPinnedPrivateOutput(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"first\n", "second\n"} {
		if err = o.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
		got, err := ReadPrivate(path)
		if err != nil || string(got) != body {
			t.Fatalf("output=%q %v", got, err)
		}
	}
	if err = o.Write(make([]byte, MaxJSONBytes+1)); err == nil {
		t.Fatal("oversized output accepted")
	}
	if err = o.Close(); err != nil {
		t.Fatal(err)
	}
	if err = o.Write([]byte("closed")); err == nil {
		t.Fatal("closed output accepted")
	}
	got, err := ReadPrivate(path)
	if err != nil || string(got) != "second\n" {
		t.Fatalf("output changed after failure=%q %v", got, err)
	}
}

func TestPinnedPrivateOutputParentReplacementCannotWriteSource(t *testing.T) {
	root := pinnedOutputTestRoot(t)
	source, reports := filepath.Join(root, "source"), filepath.Join(root, "reports")
	for _, path := range []string{source, reports} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	before := []byte("retain synthetic source exactly\n")
	if err := os.WriteFile(filepath.Join(source, "note.md"), before, 0600); err != nil {
		t.Fatal(err)
	}
	o, err := OpenPinnedPrivateOutput(filepath.Join(reports, "note.md"))
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if err = os.Rename(reports, reports+"-original"); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(source, reports); err != nil {
		t.Skip("symlink unavailable", err)
	}
	if err = o.Write([]byte("must not become source")); err == nil {
		t.Fatal("replaced parent accepted")
	}
	got, err := os.ReadFile(filepath.Join(source, "note.md"))
	if err != nil || string(got) != string(before) {
		t.Fatalf("source overwritten=%q %v", got, err)
	}
	entries, err := os.ReadDir(source)
	if err != nil || len(entries) != 1 {
		t.Fatalf("source received temporary output=%v %v", entries, err)
	}
}

func TestPinnedPrivateOutputRefusesSymlinkAndDirectory(t *testing.T) {
	root := pinnedOutputTestRoot(t)
	if _, err := OpenPinnedPrivateOutput(root); err == nil {
		t.Fatal("directory output accepted")
	}
	target := filepath.Join(root, "target.json")
	if err := WritePrivate(target, []byte("retain\n")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.json")
	if err := os.Symlink(target, link); err == nil {
		if _, err = OpenPinnedPrivateOutput(link); err == nil {
			t.Fatal("symlink output accepted")
		}
	}
	parentLink := filepath.Join(root, "parent-link")
	if err := os.Symlink(root, parentLink); err == nil {
		if _, err = OpenPinnedPrivateOutput(filepath.Join(parentLink, "result.json")); err == nil {
			t.Fatal("symlink parent accepted")
		}
	}
	got, err := ReadPrivate(target)
	if err != nil || string(got) != "retain\n" {
		t.Fatalf("target overwritten=%q %v", got, err)
	}
}

func TestPinnedStateSiblingsRetainOneRootAndOriginalLock(t *testing.T) {
	root := pinnedOutputTestRoot(t)
	o, err := OpenPinnedPrivateOutput(filepath.Join(root, "import.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if err = o.saveState(map[string]int{"batch": 1}); err != nil {
		t.Fatal(err)
	}
	s, err := o.sibling("scene.push.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.saveState(map[string]int{"item": 2}); err != nil {
		t.Fatal(err)
	}
	var got map[string]int
	if err = s.loadState(&got); err != nil || got["item"] != 2 {
		t.Fatalf("load=%v %v", got, err)
	}
	unlock, err := s.lockState()
	if err != nil {
		t.Fatal(err)
	}
	other, err := o.sibling("scene.push.json")
	if err != nil {
		unlock()
		t.Fatal(err)
	}
	if release, err := other.lockState(); err == nil {
		release()
		unlock()
		t.Fatal("same original lock was bypassed")
	}
	unlock()
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if err = o.saveState(map[string]int{"batch": 3}); err != nil {
		t.Fatal("sibling closed owner root", err)
	}
	if err = other.loadState(&got); err != nil || got["item"] != 2 {
		t.Fatalf("remaining sibling=%v %v", got, err)
	}
	for _, name := range []string{"../escape.json", ".", ""} {
		if _, err = o.sibling(name); err == nil {
			t.Fatal("unsafe sibling accepted", name)
		}
	}
}
