package provenance

import (
	"errors"
	"testing"

	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/commands"
)

func TestRecoveryKeepsUnacceptedEvidenceSeparateAndReusesIntent(t *testing.T) {
	f := setup(t)
	v := f.add(manifest.Rights{})
	req := f.req(v)
	f.files.failAppend = errors.New("injected file failure")
	if _, err := f.s.AppendEvidence(t.Context(), req); err == nil {
		t.Fatal("injected failure ignored")
	}
	inv, err := f.s.RecoveryInventory(t.Context())
	if err != nil || len(inv.Open) != 1 || len(inv.Records) != 0 {
		t.Fatalf("pending inventory: %+v %v", inv, err)
	}
	id, recordID := inv.Open[0].Operation.OperationID, inv.Open[0].Record.RecordID
	f.files.failAppend = nil
	// An installed file by itself still does not make accepted provenance.
	if _, err = f.files.AppendRecord(t.Context(), inv.Open[0].Record); err != nil {
		t.Fatal(err)
	}
	if revision, err := f.s.CurrentRevision(t.Context(), v.VersionID); err != nil || revision != 0 {
		t.Fatalf("unaccepted bytes advanced rights: %d %v", revision, err)
	}
	if _, err = f.s.RecoverOperation(t.Context(), f.who, id); err == nil {
		t.Fatal("recovered outside maintenance")
	}
	lctx, held, err := f.s.Gate.Maintain(t.Context(), commands.ReasonRecovering)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	stale := f.who
	stale.RecoveryEpoch++
	if _, err = f.s.RecoverOperation(lctx, stale, id); err == nil {
		t.Fatal("recovery replaced original epoch")
	}
	got, err := f.s.RecoverOperation(lctx, f.who, id)
	if err != nil || got.OperationID != id || got.RecordID != recordID {
		t.Fatalf("resume original: %+v %v", got, err)
	}
	inv, err = f.s.RecoveryInventory(lctx)
	if err != nil || len(inv.Open) != 0 || len(inv.Records) != 1 || len(inv.Findings) != 0 {
		t.Fatalf("accepted inventory: %+v %v", inv, err)
	}
	f.files.records[recordID] = []byte(`{"corrupt":true}`)
	inv, err = f.s.RecoveryInventory(lctx)
	if err != nil || len(inv.Findings) != 1 || inv.Findings[0].Reason != "evidence_digest" {
		t.Fatalf("corruption: %+v %v", inv, err)
	}
}

func TestRecoveryInventoryRejectsMissingAcceptedRevisionPointer(t *testing.T) {
	f := setup(t)
	v := f.add(manifest.Rights{})
	if _, err := f.s.AppendEvidence(t.Context(), f.req(v)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.ExecContext(t.Context(), `DELETE FROM provenance_targets WHERE version_id=?`, v.VersionID); err != nil {
		t.Fatal(err)
	}
	inv, err := f.s.RecoveryInventory(t.Context())
	if err != nil || len(inv.Findings) != 1 || inv.Findings[0].Reason != "evidence_current_revision_missing" {
		t.Fatalf("missing authority pointer: %+v %v", inv, err)
	}
}
