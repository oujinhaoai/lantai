package provenance

import (
	"errors"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

func assertionFixture(t *testing.T) (*fixture, AssertionRequest) {
	t.Helper()
	f := setup(t)
	f.az.Grant(f.who.PrincipalID, f.project, "provenance.restrict")
	v := f.add(manifest.Rights{})
	evidence, err := f.s.AppendEvidence(t.Context(), f.req(v))
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	return f, AssertionRequest{Subject: v.Ref(f.instance), ManifestDigest: v.ManifestDigest, ExpectedRevision: 1, Kind: "restrict", Fields: AssertionFields{NoAI: &yes}, EvidenceIDs: []ids.ID{evidence.RecordID}, Reason: "synthetic restriction"}
}
func TestAssertionPreparedRecoveryAndAtomicAcceptance(t *testing.T) {
	f, in := assertionFixture(t)
	ctx := t.Context()
	epoch, err := f.s.RightsEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.files.failAppend = errors.New("synthetic file interruption")
	if _, err = f.s.ApplyAssertion(ctx, f.who, "assertion-recovery", in); err == nil {
		t.Fatal("missing file failure")
	}
	inventory, err := f.s.RecoveryInventory(ctx)
	if err != nil || len(inventory.Open) != 1 || inventory.Open[0].Assertion == nil || len(inventory.Findings) != 0 {
		t.Fatal(inventory, err)
	}
	op := inventory.Open[0].Operation.OperationID
	preparedID := inventory.Open[0].Record.RecordID
	f.files.failAppend = nil
	if _, err = f.db.Exec(`CREATE TRIGGER fail_assertion_accept BEFORE INSERT ON provenance_assertions BEGIN SELECT RAISE(ABORT,'synthetic acceptance interruption'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.ApplyAssertion(ctx, f.who, "assertion-recovery", in); err == nil {
		t.Fatal("missing transaction failure")
	}
	current, err := f.s.RightsEpoch(ctx)
	if err != nil || current != epoch {
		t.Fatal(current, epoch, err)
	}
	d, err := f.s.EvaluateUse(ctx, f.who, in.Subject, authz.PurposeGenerativeInput)
	if err != nil || !d.Allowed {
		t.Fatal("unaccepted bytes changed policy", d, err)
	}
	if _, err = f.db.Exec(`DROP TRIGGER fail_assertion_accept`); err != nil {
		t.Fatal(err)
	}
	mctx, h, err := f.s.Gate.Maintain(ctx, commands.ReasonRecovering)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.s.RecoverOperation(mctx, f.who, op)
	h.Release()
	f.s.Gate.Open()
	if err != nil || got.RecordID != preparedID || got.OperationID != op {
		t.Fatal(got, err)
	}
	replay, err := f.s.ApplyAssertion(ctx, f.who, "assertion-recovery", in)
	if err != nil || replay.RecordID != got.RecordID {
		t.Fatal(replay, err)
	}
	inventory, err = f.s.RecoveryInventory(ctx)
	if err != nil || len(inventory.Open) != 0 || len(inventory.Records) != 2 || len(inventory.Findings) != 0 {
		t.Fatal(inventory, err)
	}
	// Missing or changed accepted bytes are findings and fail closed for use.
	f.files.records[got.RecordID] = []byte(`{}`)
	inventory, err = f.s.RecoveryInventory(ctx)
	if err != nil || len(inventory.Findings) == 0 {
		t.Fatal(inventory, err)
	}
	d, err = f.s.EvaluateUse(ctx, f.who, in.Subject, authz.PurposeGenerativeInput)
	if err != nil || d.Allowed || d.Code != errcode.RightsPending {
		t.Fatal(d, err)
	}
}
func TestAssertionRechecksAuthorityAndInstalledBytes(t *testing.T) {
	for _, scenario := range []string{"revoke", "expiry", "epoch", "tamper"} {
		t.Run(scenario, func(t *testing.T) {
			f, in := assertionFixture(t)
			ctx := t.Context()
			f.files.afterAppend = func() {
				switch scenario {
				case "revoke":
					f.az.Revoke(f.who.PrincipalID, f.project)
				case "expiry":
					f.clk.Advance(2 * time.Hour)
				case "epoch":
					f.az.Restore()
				case "tamper":
					for id := range f.files.records {
						if id != in.EvidenceIDs[0] {
							f.files.records[id] = []byte(`{}`)
						}
					}
				}
			}
			if _, err := f.s.ApplyAssertion(ctx, f.who, "assertion-final-check", in); err == nil {
				t.Fatal("invalid final acceptance")
			}
			var n int
			if err := f.db.QueryRow(`SELECT count(*) FROM provenance_assertions`).Scan(&n); err != nil || n != 0 {
				t.Fatal(n, err)
			}
			if len(f.files.records) != 2 {
				t.Fatal("unaccepted evidence not retained")
			}
		})
	}
}
func TestAssertionEvidenceAppendRequiresCurrentPersonalAccess(t *testing.T) {
	f, in := assertionFixture(t)
	ctx := t.Context()
	personal := "personal"
	in.Fields = AssertionFields{Sensitivity: &personal}
	if _, err := f.s.ApplyAssertion(ctx, f.who, "personal-restriction", in); err != nil {
		t.Fatal(err)
	}
	v := f.reader.versions[in.Subject.VersionID]
	request := f.req(v)
	request.ExpectedRevision = 1
	if _, err := f.s.AppendEvidence(ctx, request); errcode.CodeOf(err) != errcode.NotFound {
		t.Fatal("personal evidence exposed", err)
	}
	f.az.Grant(f.who.PrincipalID, f.project, ActionPersonalRead)
	if _, err := f.s.AppendEvidence(ctx, request); err != nil {
		t.Fatal(err)
	}
}

func TestAssertionCancellationFencesInFlightAcceptance(t *testing.T) {
	f, in := assertionFixture(t)
	ctx := t.Context()
	f.az.Grant(f.who.PrincipalID, f.project, "provenance.cancel_assertion")
	var cancellation CancelAssertionRequest
	var result CancelAssertionReceipt
	f.files.afterAppend = func() {
		inventory, err := f.s.RecoveryInventory(ctx)
		if err != nil || len(inventory.Open) != 1 {
			t.Fatal(inventory, err)
		}
		original := inventory.Open[0].Operation
		cancellation = CancelAssertionRequest{ProjectID: f.project, OperationID: original.OperationID, RequestHash: original.RequestHash, Reason: "cancel unaccepted test intent"}
		result, err = f.s.CancelAssertion(ctx, f.who, "cancel-intent", cancellation)
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := f.s.ApplyAssertion(ctx, f.who, "cancelled-in-flight", in)
	code(t, err, errcode.InvalidStateTransition)
	replay, err := f.s.CancelAssertion(ctx, f.who, "cancel-intent", cancellation)
	if err != nil || replay.OperationID != result.OperationID {
		t.Fatal(replay, err)
	}
	_, err = f.s.ApplyAssertion(ctx, f.who, "cancelled-in-flight", in)
	code(t, err, errcode.InvalidStateTransition)
	inventory, err := f.s.RecoveryInventory(ctx)
	if err != nil || len(inventory.Open) != 0 || len(inventory.Records) != 1 || len(f.files.records) != 2 {
		t.Fatal(inventory, err)
	}
	d, err := f.s.EvaluateUse(ctx, f.who, in.Subject, authz.PurposeGenerativeInput)
	if err != nil || !d.Allowed {
		t.Fatal(d, err)
	}
	// A new explicit operation can still be accepted; its completed restriction
	// cannot be undone by the cancellation command.
	accepted, err := f.s.ApplyAssertion(ctx, f.who, "new-after-cancel", in)
	if err != nil {
		t.Fatal(err)
	}
	op, err := f.s.store.GetOperation(ctx, f.db, accepted.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.CancelAssertion(ctx, f.who, "cancel-committed", CancelAssertionRequest{ProjectID: f.project, OperationID: op.OperationID, RequestHash: op.RequestHash, Reason: "must not undo active restriction"})
	code(t, err, errcode.InvalidStateTransition)
}
