package ledger

import (
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
)

func TestCommitPinsSurviveReopenBlockedAndCommit(t *testing.T) {
	f := newFixture(t)
	p, proof := f.prepare(t, "pending/pin")
	sha := p.Files[0].SHA256
	pins, err := f.s.PinsFor(t.Context(), sha)
	if err != nil || len(pins) != 1 || !pins[0].ActiveAt(f.clk.Now().Add(365*24*time.Hour)) {
		t.Fatalf("prepared pin: %+v %v", pins, err)
	}
	id := pins[0].PinID
	if err = f.s.changeStage(t.Context(), p.OperationID, commands.StageBlocked, errcode.New(errcode.OperationNeedsReconciliation, "pending")); err != nil {
		t.Fatal(err)
	}
	f.reopen(t)
	pins, err = f.s.PinsFor(t.Context(), sha)
	if err != nil || len(pins) != 1 || pins[0].PinID != id || !pins[0].ActiveAt(f.clk.Now().Add(365*24*time.Hour)) {
		t.Fatalf("reopened blocked pin: %+v %v", pins, err)
	}
	inv, err := f.s.RecoveryInventory(t.Context())
	if err != nil || len(inv.Open) != 1 || inv.Open[0].PreparedVersion == nil || inv.Open[0].Command.RecoveryEpoch != f.who.RecoveryEpoch || len(inv.Pins) != 1 {
		t.Fatalf("recovery inventory: %+v %v", inv, err)
	}
	// Concurrent inspection may see either side of the final transaction, but
	// must always return the same pin backed by a durable operation.
	done := make(chan error, 1)
	go func() { _, e := f.s.Commit(t.Context(), p.OperationID, f.who, proof); done <- e }()
	for range 10 {
		got, e := f.s.PinsFor(t.Context(), sha)
		if e != nil || len(got) != 1 || got[0].PinID != id {
			t.Fatalf("concurrent pin: %+v %v", got, e)
		}
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	pins, err = f.s.PinsFor(t.Context(), sha)
	if err != nil || len(pins) != 1 || pins[0].ReleasedAt.IsZero() || pins[0].ActiveAt(f.clk.Now()) {
		t.Fatalf("committed release: %+v %v", pins, err)
	}
	f.reopen(t)
	got, err := f.s.PinsFor(t.Context(), sha)
	if err != nil || len(got) != 1 || got[0].ReleasedAt != pins[0].ReleasedAt {
		t.Fatalf("release not stable: %+v %v", got, err)
	}
}

func TestRecoveryInventoryIncludesHistoricalMetadataAndCancellationPins(t *testing.T) {
	f := newFixture(t)
	p, _ := f.prepare(t, "cancel/pin")
	if err := f.s.Cancel(t.Context(), p.OperationID, f.who); err != nil {
		t.Fatal(err)
	}
	pins, err := f.s.PinsFor(t.Context(), p.Files[0].SHA256)
	if err != nil || len(pins) != 1 || pins[0].ReleasedAt.IsZero() {
		t.Fatalf("cancelled pin: %+v %v", pins, err)
	}
	target := commit.MetadataTarget{Kind: commit.TargetProject, ProjectID: f.project, ID: f.project}
	for i := int64(0); i < 2; i++ {
		r := commit.MetadataRequest{Who: f.who, Target: target, ExpectedRevision: i, ContentDigest: digest.Of([]byte{byte(i)}), Action: "catalog.patch_project"}
		key := "meta.first"
		if i == 1 {
			key = "meta.second"
		}
		pm, err := f.s.PrepareMetadata(t.Context(), f.command(commit.CommandCommitMetadata, f.project, key, r), r)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.s.CommitMetadata(t.Context(), pm.OperationID, f.who, f.rev.Write(pm)); err != nil {
			t.Fatal(err)
		}
	}
	inv, err := f.s.RecoveryInventory(t.Context())
	if err != nil || len(inv.Metadata) != 2 || len(inv.Open) != 0 {
		t.Fatalf("historical references: %+v %v", inv, err)
	}
}

func TestCommitPinFailsClosedWhenIntentFactMissing(t *testing.T) {
	f := newFixture(t)
	p, _ := f.prepare(t, "missing/pin")
	if _, err := f.db.Exec(`DELETE FROM operations WHERE operation_id=?`, p.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.PinsFor(t.Context(), p.Files[0].SHA256); err == nil {
		t.Fatal("missing operation silently lost retention")
	}
}
