package client

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestImportRootAliasesAreNotDisjoint(t *testing.T) {
	root := worksetTemp(t)
	for _, pair := range [][2]string{
		{filepath.Join(root, "Source"), filepath.Join(root, "SOURCE", "state")},
		{filepath.Join(root, "Caf\u00e9"), filepath.Join(root, "Cafe\u0301", "verify")},
		{filepath.Join(root, "Stra\u00dfe"), filepath.Join(root, "STRASSE", "state")},
	} {
		if err := importDisjoint(pair[0], pair[1]); err == nil {
			t.Fatalf("portable aliases accepted as separate roots: %q %q", pair[0], pair[1])
		}
	}
	if err := importDisjoint(filepath.Join(root, "source"), filepath.Join(root, "state"), filepath.Join(root, "verified")); err != nil {
		t.Fatal(err)
	}
}

func TestImportRejectsCaseAliasRootsBeforeWrites(t *testing.T) {
	original, in := importFixture(t)
	parent := worksetTemp(t)
	source := filepath.Join(parent, "Source")
	if err := os.Rename(original, source); err != nil {
		t.Fatal(err)
	}
	plan := importMustPlan(t, source, in)
	var calls atomic.Int64
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(500) })
	alias := filepath.Join(parent, "SOURCE")
	if _, err := c.ApplyImport(t.Context(), plan, source, filepath.Join(alias, "state")); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("case alias apply: %v", err)
	}
	state := filepath.Join(parent, "private-state")
	report := ImportReport{Schema: ImportReportSchema, Origin: c.Origin(), PlanDigest: plan.Digest, Results: []ImportResult{{Key: in.Assets[0].Key, Status: "not_attempted"}}}
	if _, err := c.VerifyImport(t.Context(), plan, report, source, state, filepath.Join(alias, "verified"), "archive_review"); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("case alias verify: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("unsafe roots reached HTTP")
	}
	for _, path := range []string{filepath.Join(source, "state"), filepath.Join(source, "verified"), state} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("unsafe root created before rejection: %s %v", path, err)
		}
	}
}
