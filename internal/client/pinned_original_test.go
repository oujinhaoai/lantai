package client

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestOriginalOutputConcurrentPlansNeverReplaceEachOther(t *testing.T) {
	root := pinnedOutputTestRoot(t)
	path := filepath.Join(root, "original.json")
	first, err := OpenPinnedPrivateOutput(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenPinnedPrivateOutput(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	values := [][]byte{[]byte(`{"plan":"first"}`), []byte(`{"plan":"second"}`)}
	errors := make([]error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, output := range []*PinnedPrivateOutput{first, second} {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; errors[i] = output.WriteOriginal(values[i]) }()
	}
	close(start)
	wg.Wait()
	if (errors[0] == nil) == (errors[1] == nil) {
		t.Fatal("expected exactly one accepted original", errors)
	}
	winner := 0
	if errors[0] != nil {
		winner = 1
	}
	raw, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, values[winner]) {
		t.Fatal("winning original changed", string(raw), err)
	}
	if err := first.WriteOriginal(raw); err != nil {
		t.Fatal("byte-identical resume failed", err)
	}
	if err := first.WriteOriginal([]byte(`{"plan":"replacement"}`)); err == nil {
		t.Fatal("different original replaced")
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, raw) {
		t.Fatal("rejection changed original", err)
	}
}

func TestBatchOutputOutsideSelectedRoots(t *testing.T) {
	root := pinnedOutputTestRoot(t)
	source := filepath.Join(root, "selected")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{filepath.Join(source, "plan.json"), filepath.Join(root, "SELECTED", "plan.json")} {
		if p, err := OpenBatchPrivateOutput(output, source); err == nil {
			p.Close()
			t.Fatal("source alias accepted as output", output)
		}
	}
	input := filepath.Join(root, "input.json")
	if err := WritePrivate(input, []byte(`{"original":true}`)); err != nil {
		t.Fatal(err)
	}
	if p, err := OpenBatchPrivateOutput(input, input); err == nil {
		p.Close()
		t.Fatal("input accepted as output")
	}
	output, err := OpenBatchPrivateOutput(filepath.Join(root, "plan.json"), source, input)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	if err := output.WriteOriginal([]byte(`{"plan":true}`)); err != nil {
		t.Fatal(err)
	}
}
