package storage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/install"
	"github.com/oujinhaoai/lantai/internal/storage/fileop"
)

func TestUnmovedTrashVerifierChecksBytesAndReadyJournal(t *testing.T) {
	faults := &fileop.Faults{}
	f, req := installFixture(t, faults)
	ctx := t.Context()
	proof, err := f.svc.Install(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	plan := FileIntent{OperationID: ids.New(), TrashID: ids.New(), Action: "trash", Versions: []install.Proof{proof}}
	if err = f.svc.VerifyUnmovedTrash(ctx, plan); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(f.svc.layout.VersionDir(proof.ProjectID, proof.AssetID, proof.VersionNumber), "unlisted.txt")
	if err = os.WriteFile(unknown, []byte("synthetic unexpected bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = f.svc.VerifyUnmovedTrash(ctx, plan); errcode.CodeOf(err) != errcode.OperationNeedsReconciliation {
		t.Fatal("unlisted source content accepted", err)
	}
	if err = os.Remove(unknown); err != nil {
		t.Fatal(err)
	}
	faults.Rename = func(string, string) error { return errors.New("synthetic failure before rename") }
	if err = f.svc.ApplyFileIntent(ctx, plan.OperationID, &intentSource{plan: plan}); err == nil {
		t.Fatal("rename fault not injected")
	}
	var state string
	if err = f.svc.db.QueryRowContext(ctx, `SELECT state FROM storage_file_actions WHERE operation_id=?`, plan.OperationID).Scan(&state); err != nil || state != "ready" {
		t.Fatal(state, err)
	}
	if err = f.svc.VerifyUnmovedTrash(ctx, plan); err != nil {
		t.Fatal("unmoved ready journal could not be reconciled", err)
	}
	faults.Rename = nil
	if err = f.svc.ApplyFileIntent(ctx, plan.OperationID, &intentSource{plan: plan}); err != nil {
		t.Fatal(err)
	}
	if err = f.svc.VerifyUnmovedTrash(ctx, plan); errcode.CodeOf(err) != errcode.OperationNeedsReconciliation {
		t.Fatal("completed move treated as unmoved", err)
	}
}
