package integration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

func TestM2TrashInventoryPreservesOrphansAndChecksRecordLocation(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	who := e.login().Context
	v := e.ingest(who, "trash-record-inventory", []byte("synthetic inventory content"), *rightsOwned())
	accepted := rightsEvidence(t, e, who, v, false)
	orphan := storage.Record{RecordID: ids.New(), ProjectID: v.ProjectID, AssetID: v.AssetID, VersionID: v.VersionID, ManifestDigest: v.ManifestDigest, Kind: "note", Payload: []byte(`{"note":"synthetic unaccepted evidence"}`), AuthorID: who.PrincipalID, OperationID: ids.New(), CreatedAt: clock.Format(e.clk.Now())}
	if _, err := e.storage.AppendRecord(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	l, err := e.ledger.NewLifecycle(e.storage, lifecycleUsesFixture{e}, e.rights, e.id, e.catalog)
	if err != nil {
		t.Fatal(err)
	}
	entry := trashForTest(t, e, l, who, v.AssetID)
	app := &application.App{Instance: e.inst, Ledger: e.ledger, Storage: e.storage, Catalog: e.catalog, Rights: e.rights}
	before, err := app.FSCK(ctx, true)
	if err != nil || before.Err() != nil || len(before.Storage.Records) != 2 {
		t.Fatal(before.Findings, before.Storage.Records, err)
	}
	count := 0
	for _, finding := range before.Residuals {
		if finding.Reason == "unaccepted_record" && finding.OperationID == orphan.OperationID {
			count++
		}
		if finding.OperationID == accepted.OperationID {
			t.Fatal("accepted trash evidence was treated as an orphan", finding)
		}
	}
	if count != 1 {
		t.Fatal("trash orphan was missed or duplicated", before.Residuals)
	}
	recordDir := filepath.Join(e.inst.Layout().Home, "trash", string(entry.ID), "records", string(v.VersionID))
	orphanPath := filepath.Join(recordDir, string(orphan.RecordID)+".json")
	if _, err = os.Stat(orphanPath); err != nil {
		t.Fatal("inventory removed orphan bytes", err)
	}
	// A byte-for-byte copy of accepted evidence under another trash entry is
	// still invalid: acceptance is not authority to invent its physical location.
	acceptedBytes, err := os.ReadFile(filepath.Join(recordDir, string(accepted.RecordID)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	wrongPath := filepath.Join(e.inst.Layout().Home, "trash", string(ids.New()), "records", string(v.VersionID), string(accepted.RecordID)+".json")
	if err = os.MkdirAll(filepath.Dir(wrongPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(wrongPath, acceptedBytes, 0600); err != nil {
		t.Fatal(err)
	}
	wrong, err := app.FSCK(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, finding := range wrong.Findings {
		found = found || finding.Reason == "record_location_mismatch" && finding.OperationID == accepted.OperationID
	}
	if !found || wrong.Err() == nil {
		t.Fatal("accepted bytes in the wrong trash entry were trusted", wrong.Findings)
	}
	if _, err = os.Stat(wrongPath); err != nil {
		t.Fatal("inventory removed misplaced evidence", err)
	}
	if err = os.Remove(wrongPath); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(orphanPath, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(orphanPath, []byte("synthetic malformed record"), 0600); err != nil {
		t.Fatal(err)
	}
	malformed, err := app.FSCK(ctx, true)
	if err != nil || malformed.Err() != nil {
		t.Fatal(malformed.Findings, err)
	}
	found = false
	for _, finding := range malformed.Residuals {
		found = found || finding.Reason == "record_invalid"
	}
	if !found {
		t.Fatal("malformed trash orphan was omitted", malformed.Residuals)
	}
	if _, err = os.Stat(orphanPath); err != nil {
		t.Fatal("inventory removed malformed bytes", err)
	}
}
