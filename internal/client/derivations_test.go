package client

import (
	"bytes"
	"context"
	"fmt"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPinnedBinaryArtifactKeepsJSONBudgetAndOriginalBytes(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "selected.tar")
	output, err := OpenPinnedPrivateOutput(path)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	large := bytes.Repeat([]byte{3}, int(MaxJSONBytes)+1)
	if err := output.WriteOriginal(large); err == nil {
		t.Fatal("binary export widened JSON state budget")
	}
	if err := output.WriteBinaryOriginal(large); err != nil {
		t.Fatal(err)
	}
	if err := output.WriteBinaryOriginal(large); err != nil {
		t.Fatal("same original binary could not resume", err)
	}
	large[0] = 4
	if err := output.WriteBinaryOriginal(large); err == nil {
		t.Fatal("binary output overwrote different original")
	}
	actual, err := os.ReadFile(path)
	if err != nil || actual[0] != 3 || len(actual) != int(MaxJSONBytes)+1 {
		t.Fatal("original binary changed", err)
	}
	if _, err := ReadPrivate(path); err == nil {
		t.Fatal("binary artifact became an oversized private JSON credential")
	}
}
func TestDerivationCancellationBeforePublication(t *testing.T) {
	c, err := New(Config{BaseURL: "http://127.0.0.1:1", AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	output, err := OpenPinnedPrivateOutput(filepath.Join(root, "artifact"))
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.DeriveTo(ctx, DerivationInput{Schema: DerivationInputSchema, Action: "workset_tar"}, root, output); err != context.Canceled {
		t.Fatal("cancelled derivation started IO", err)
	}
	if _, err := os.Stat(filepath.Join(root, "artifact")); !os.IsNotExist(err) {
		t.Fatal("cancelled derivation published", err)
	}
}

func TestDerivationArchiveBudgetsBeforeGrantsOrCachedHashing(t *testing.T) {
	for _, kind := range []string{"bytes", "files"} {
		t.Run(kind, func(t *testing.T) {
			f := newWorksetFixture(t)
			files := []InputFile{{Path: "large.bin", Role: "source", SHA256: sha(""), Size: DerivationBytes + 1}}
			if kind == "files" {
				files = nil
				for i := range DerivationFiles + 1 {
					files = append(files, InputFile{Path: fmt.Sprintf("f%d", i), Role: "source", SHA256: sha(""), Size: 0})
				}
			}
			entry := f.entryWithSpecs(WorksetEntry{AssetID: string(ids.New()), VersionID: string(ids.New()), Destination: "selected"}, files)
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			directory := filepath.Join(root, "cache")
			if err := os.Mkdir(directory, 0700); err != nil {
				t.Fatal(err)
			}
			artifact, err := OpenPinnedPrivateOutput(filepath.Join(root, "out.tar"))
			if err != nil {
				t.Fatal(err)
			}
			defer artifact.Close()
			input := WorksetInput{Schema: WorksetSchema, Entries: []WorksetEntry{entry}}
			_, err = f.c.DeriveTo(t.Context(), DerivationInput{Schema: DerivationInputSchema, Action: "workset_tar", Purpose: "raw_export", Workset: &input}, directory, artifact)
			if err == nil || !strings.Contains(err.Error(), "budget") {
				t.Fatal("archive budget did not reject before transfer", err)
			}
			for _, request := range f.requests {
				if strings.Contains(request, "read-grants") || strings.Contains(request, "/xfer/") {
					t.Fatal("over-budget archive transferred", request)
				}
			}
			if _, err := os.Stat(filepath.Join(root, "out.tar")); !os.IsNotExist(err) {
				t.Fatal("over-budget archive published", err)
			}
		})
	}
}
