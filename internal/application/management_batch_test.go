package application

import (
	"context"
	"testing"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

func TestBatchAcceptanceBoundsZeroByteFilesAcrossVersions(t *testing.T) {
	budget := &batchReadBudget{}
	ctx := context.WithValue(t.Context(), batchBudgetKey{}, budget)
	version := func(n int) catalog.VersionInfo {
		return catalog.VersionInfo{Manifest: manifest.Document{Content: manifest.Content{Files: make([]manifest.File, n)}}}
	}
	if err := reserveBatchManifest(ctx, version(9000)); err != nil {
		t.Fatal(err)
	}
	if err := reserveBatchManifest(ctx, version(1000)); err != nil {
		t.Fatal(err)
	}
	if err := reserveBatchManifest(ctx, version(1)); errcode.CodeOf(err) != errcode.QuotaExceeded {
		t.Fatalf("zero-byte file count escaped global budget: %v", err)
	}
	if budget.bytes != 0 || budget.files != 10000 {
		t.Fatalf("zero-byte budget changed incorrectly: %+v", budget)
	}
}
