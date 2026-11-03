package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The same test executes the native ACL check on Windows and0600/owner checks
// on Unix, including a partial left by a real interrupted HTTP response.
func TestPinnedDownloadCreatesPrivateStatePartialAndFinal(t *testing.T) {
	f := newWorksetFixture(t)
	f.shortOnce = true
	data := strings.Repeat("synthetic private record\n", 300)
	entry := f.entry("selected", map[string]string{"nested/record.json": data})
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	input := WorksetInput{Schema: WorksetSchema, Entries: []WorksetEntry{entry}}
	if _, err := f.c.PullWorkset(t.Context(), input, root, "production"); err == nil {
		t.Fatal("truncated response accepted")
	}
	check := func(name string) {
		t.Helper()
		file, err := os.Open(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if err := checkPinnedOutputPrivate(file); err != nil {
			t.Fatalf("download not private at %s: %v", name, err)
		}
	}
	path := "selected/nested/record.json"
	check(path + ".lantai-part")
	check(path + ".lantai-download.json")
	if _, err := f.c.PullWorkset(t.Context(), input, root, "production"); err != nil {
		t.Fatal("original private download could not resume", err)
	}
	check(path)
	raw, err := ReadPrivate(filepath.Join(root, path))
	if err != nil || string(raw) != data {
		t.Fatal("final record bytes/private identity differ", err)
	}
	for _, suffix := range []string{".lantai-part", ".lantai-download.json"} {
		if _, err := os.Stat(filepath.Join(root, path+suffix)); !os.IsNotExist(err) {
			t.Fatal("completed partial state not cleared", err)
		}
	}
}
