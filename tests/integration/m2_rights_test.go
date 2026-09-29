package integration

import (
	"context"
	"encoding/json"
	"github.com/oujinhaoai/lantai/internal/commands"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/provenance"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

func rightsEvidence(t *testing.T, e *env, who authz.Context, v catalog.VersionResult, unknown bool) provenance.AcceptedRecord {
	t.Helper()
	rev, err := e.rights.CurrentRevision(t.Context(), v.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	inputs := []provenance.ExternalInput{}
	if unknown {
		inputs = append(inputs, provenance.ExternalInput{RightsStatus: "unknown", SourceURL: "https://example.invalid/synthetic-evidence"})
	}
	result, err := e.rights.AppendEvidence(t.Context(), provenance.AppendRequest{Who: who, IdempotencyKey: e.key(), Ref: v.Ref, ManifestDigest: v.ManifestDigest, ExpectedRevision: rev, Evidence: provenance.Evidence{Note: "synthetic supporting evidence", ExternalInputs: inputs}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func humanAssertionForTest(t *testing.T, e *env, who authz.Context, in provenance.AssertionRequest) (provenance.AssertionReceipt, error) {
	t.Helper()
	ctx := t.Context()
	action, err := e.rights.AssertionHumanAction(ctx, in)
	if err != nil {
		return provenance.AssertionReceipt{}, err
	}
	ch, err := e.id.CreateDomainChallenge(ctx, who, []identity.HumanAction{action}, e.rights)
	if err != nil {
		return provenance.AssertionReceipt{}, err
	}
	grant, err := e.id.VerifyChallenge(ctx, who, ch.ChallengeID, e.fresh(), "192.0.2.10")
	if err != nil {
		return provenance.AssertionReceipt{}, err
	}
	items, err := e.id.DomainItems(ctx, who, grant.GrantID)
	if err != nil {
		return provenance.AssertionReceipt{}, err
	}
	return e.rights.ApplyHumanAssertion(ctx, who, in, grant.GrantID, items[0].OperationID, e.id)
}
func TestM2RightsAssertionsPropagateAndRequireNewHumanEvidence(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	who := e.login().Context
	source := e.ingest(who, "rights-source", []byte("synthetic rights source"), *rightsOwned())
	target := e.ingest(who, "rights-derived", []byte("synthetic derivative"), *rightsOwned(), catalog.DeclaredUse{AssetID: source.AssetID, VersionID: source.VersionID, Relation: "derived_from"})
	before, err := e.rights.EvaluateUse(ctx, who, target.Ref, authz.PurposeGenerativeInput)
	if err != nil || !before.Allowed {
		t.Fatal(before, err)
	}
	evidence := rightsEvidence(t, e, who, source, false)
	yes, no := true, false
	req := provenance.AssertionRequest{Subject: source.Ref, ManifestDigest: source.ManifestDigest, ExpectedRevision: 1, Kind: "restrict", Fields: provenance.AssertionFields{NoAI: &yes}, EvidenceIDs: []ids.ID{evidence.RecordID}, Reason: "synthetic risk restriction"}
	key := e.key()
	restricted, err := e.rights.ApplyAssertion(ctx, who, key, req)
	if err != nil {
		t.Fatal(err)
	}
	after, err := e.rights.EvaluateUse(ctx, who, target.Ref, authz.PurposeGenerativeInput)
	if err != nil || after.Allowed || after.RightsEpoch <= before.RightsEpoch {
		t.Fatal(after, err)
	}
	checkDecision(t, e, who, target.Ref, authz.PurposeProduction, "")
	replay, err := e.rights.ApplyAssertion(ctx, who, key, req)
	if err != nil || replay.RecordID != restricted.RecordID {
		t.Fatal(replay, err)
	}
	lower := req
	lower.ExpectedRevision = restricted.Revision
	lower.Fields.NoAI = &no
	if _, err = e.rights.ApplyAssertion(ctx, who, e.key(), lower); errcode.CodeOf(err) != errcode.HumanProofRequired {
		t.Fatal("ordinary permission lowered policy", err)
	}
	lower.Kind = "release"
	lower.ReplacesAssertionID = restricted.RecordID
	if _, err = humanAssertionForTest(t, e, who, lower); errcode.CodeOf(err) != errcode.ChecksNotSatisfied {
		t.Fatal("old evidence reused to lower", err)
	}
	fresh := rightsEvidence(t, e, who, source, false)
	lower.EvidenceIDs = []ids.ID{fresh.RecordID}
	released, err := humanAssertionForTest(t, e, who, lower)
	if err != nil {
		t.Fatal(err)
	}
	checkDecision(t, e, who, target.Ref, authz.PurposeGenerativeInput, "")
	// The frozen manifest and old restriction record never change.
	committed, err := e.ledger.Version(ctx, source.AssetID, source.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := e.storage.ReadManifest(ctx, committed)
	if err != nil {
		t.Fatal(err)
	}
	manifestDoc, err := manifest.Parse(raw)
	if err != nil || manifestDoc.Content.Rights.NoAI {
		t.Fatal(manifestDoc, err)
	}
	old, err := e.storage.ReadRecord(ctx, e.project.ProjectID, source.AssetID, source.VersionID, restricted.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	var record storage.Record
	if err = json.Unmarshal(old, &record); err != nil {
		t.Fatal(err)
	}
	var assertion provenance.RightsAssertion
	if err = json.Unmarshal(record.Payload, &assertion); err != nil || assertion.Fields.NoAI == nil || !*assertion.Fields.NoAI {
		t.Fatal(assertion, err)
	}
	unknown := rightsEvidence(t, e, who, source, true)
	checkDecision(t, e, who, target.Ref, authz.PurposeProduction, errcode.RightsPending)
	proof := rightsEvidence(t, e, who, source, false)
	confirmed := []ids.ID{unknown.RecordID}
	confirm := provenance.AssertionRequest{Subject: source.Ref, ManifestDigest: source.ManifestDigest, ExpectedRevision: released.Revision, Kind: "release", ReplacesAssertionID: released.RecordID, Fields: provenance.AssertionFields{ConfirmedEvidenceIDs: &confirmed}, EvidenceIDs: []ids.ID{proof.RecordID}, Reason: "new evidence confirms this precise external input"}
	if _, err = humanAssertionForTest(t, e, who, confirm); err != nil {
		t.Fatal(err)
	}
	checkDecision(t, e, who, target.Ref, authz.PurposeProduction, "")
	rightsEvidence(t, e, who, source, true)
	checkDecision(t, e, who, target.Ref, authz.PurposeProduction, errcode.RightsPending)
}

func TestM2RightsCorrectionReconcilesStaleRelations(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	who := e.login().Context
	a := e.ingest(who, "correction-source", []byte("source"), *rightsOwned())
	b := e.ingest(who, "correction-derived", []byte("derived"), *rightsOwned(), catalog.DeclaredUse{AssetID: a.AssetID, VersionID: a.VersionID, Relation: "derived_from"})
	es, q := e.eventQuery()
	e.relay(es)
	if _, err := q.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	old, err := q.Relations(ctx, who, a.Ref, true)
	if err != nil || len(old) != 1 {
		t.Fatal(old, err)
	}
	proof := rightsEvidence(t, e, who, b, false)
	empty := []manifest.Use{}
	req := provenance.AssertionRequest{Subject: b.Ref, ManifestDigest: b.ManifestDigest, ExpectedRevision: 1, Kind: "correct", Fields: provenance.AssertionFields{Uses: &empty}, EvidenceIDs: []ids.ID{proof.RecordID}, Reason: "evidence corrects erroneous source declaration"}
	accepted, err := humanAssertionForTest(t, e, who, req)
	if err != nil {
		t.Fatal(err)
	}
	// No relay/CatchUp has happened: current truth still removes the stale edge.
	for _, incoming := range []bool{false, true} {
		ref := b.Ref
		if incoming {
			ref = a.Ref
		}
		got, err := q.Relations(ctx, who, ref, incoming)
		if err != nil || len(got) != 0 {
			t.Fatal(got, err)
		}
	}
	visible, err := e.rights.VisibleUses(ctx, who, b.Ref)
	if err != nil || len(visible) != 0 {
		t.Fatal(visible, err)
	}
	uses, err := e.rights.IncomingUses(ctx, []ids.PermanentRef{a.Ref})
	if err != nil || len(uses) != 0 {
		t.Fatal(uses, err)
	}
	// Add the precise edge back with new evidence; corrected graph is indexed by
	// rights.asserted and its revision participates in destructive-action previews.
	proof = rightsEvidence(t, e, who, b, false)
	edges := []manifest.Use{{InstanceID: a.Ref.InstanceID, AssetID: a.AssetID, VersionID: a.VersionID, Relation: "derived_from"}}
	req.ExpectedRevision, req.ReplacesAssertionID, req.Fields.Uses, req.EvidenceIDs = accepted.Revision, accepted.RecordID, &edges, []ids.ID{proof.RecordID}
	accepted, err = humanAssertionForTest(t, e, who, req)
	if err != nil {
		t.Fatal(err)
	}
	e.relay(es)
	if _, err = q.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := q.Relations(ctx, who, a.Ref, true)
	if err != nil || len(got) != 1 {
		t.Fatal(got, err)
	}
	uses, err = e.rights.IncomingUses(ctx, []ids.PermanentRef{a.Ref})
	if err != nil || len(uses) != 1 || uses[0].SourceRightsRevision != accepted.Revision {
		t.Fatal(uses, err)
	}
	// A human grant does not waive a dependency cycle.
	proof = rightsEvidence(t, e, who, a, false)
	cycle := []manifest.Use{{InstanceID: b.Ref.InstanceID, AssetID: b.AssetID, VersionID: b.VersionID, Relation: "uses"}}
	req = provenance.AssertionRequest{Subject: a.Ref, ManifestDigest: a.ManifestDigest, ExpectedRevision: 1, Kind: "correct", Fields: provenance.AssertionFields{Uses: &cycle}, EvidenceIDs: []ids.ID{proof.RecordID}, Reason: "must reject cyclic source declaration"}
	if _, err = humanAssertionForTest(t, e, who, req); err == nil {
		t.Fatal("cycle accepted")
	}
	inventory, err := e.rights.RecoveryInventory(ctx)
	if err != nil || len(inventory.Findings) != 0 {
		t.Fatal(inventory.Findings, err)
	}
	assertionCount := 0
	for _, r := range inventory.Records {
		if r.RecordID == accepted.RecordID {
			assertionCount++
		}
	}
	if assertionCount != 1 {
		t.Fatal("accepted assertion absent from recovery inventory")
	}
}

// A fault-injection file adapter delegates all bytes to the real storage service.
// It changes the clock only after the assertion file is durably installed.
type assertionExpiryFiles struct {
	provenance.Files
	after func()
}

func (f *assertionExpiryFiles) AppendRecord(ctx context.Context, r storage.Record) (storage.RecordRef, error) {
	result, err := f.Files.AppendRecord(ctx, r)
	if err == nil && r.Kind == "rights_assertion" && f.after != nil {
		fn := f.after
		f.after = nil
		fn()
	}
	return result, err
}
func TestM2HumanAssertionGrantExpiresDuringFileIO(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	who := e.login().Context
	rights := *rightsOwned()
	rights.NoAI = true
	v := e.ingest(who, "grant-expiry", []byte("synthetic no AI input"), rights)
	proof := rightsEvidence(t, e, who, v, false)
	no := false
	req := provenance.AssertionRequest{Subject: v.Ref, ManifestDigest: v.ManifestDigest, ExpectedRevision: 1, Kind: "release", Fields: provenance.AssertionFields{NoAI: &no}, EvidenceIDs: []ids.ID{proof.RecordID}, Reason: "new proof would allow input"}
	// This is explicit test assembly, not a runtime adapter replacement API.
	deps := e.rights.Deps
	deps.Files = &assertionExpiryFiles{Files: deps.Files, after: func() { e.clk.Advance(6 * time.Minute) }}
	var err error
	e.rights, err = provenance.New(deps)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = humanAssertionForTest(t, e, who, req); errcode.CodeOf(err) != errcode.HumanProofRequired {
		t.Fatal("expired grant accepted", err)
	}
	checkDecision(t, e, who, v.Ref, authz.PurposeGenerativeInput, errcode.UseRestricted)
	inventory, err := e.rights.RecoveryInventory(ctx)
	if err != nil || len(inventory.Open) != 1 || inventory.Open[0].Assertion == nil || len(inventory.Records) != 1 {
		t.Fatal(inventory, err)
	}
	mctx, h, err := e.inst.Gate().Maintain(ctx, commands.ReasonRecovering)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.rights.RecoverOperation(mctx, who, inventory.Open[0].Operation.OperationID)
	h.Release()
	e.inst.Gate().Open()
	if errcode.CodeOf(err) != errcode.HumanProofRequired {
		t.Fatal("maintenance reactivated expired human authority", err)
	}
	original := inventory.Open[0]
	cancel := provenance.CancelAssertionRequest{ProjectID: e.project.ProjectID, OperationID: original.Operation.OperationID, RequestHash: original.Operation.RequestHash, Reason: "expired human authorization; retain orphan for reconciliation"}
	_, err = e.rights.CancelAssertion(ctx, who, e.key(), cancel)
	if err != nil {
		t.Fatal(err)
	}
	var assertion provenance.RightsAssertion
	if err = json.Unmarshal(original.Record.Payload, &assertion); err != nil {
		t.Fatal(err)
	}
	if _, err = e.rights.ApplyHumanAssertion(ctx, who, req, assertion.HumanGrantID, original.Operation.OperationID, e.id); errcode.CodeOf(err) != errcode.InvalidStateTransition {
		t.Fatal("cancelled human operation retried", err)
	}
	inventory, err = e.rights.RecoveryInventory(ctx)
	if err != nil || len(inventory.Open) != 0 || len(inventory.Records) != 1 {
		t.Fatal(inventory, err)
	}
	if _, err = e.storage.ReadRecord(ctx, e.project.ProjectID, v.AssetID, v.VersionID, original.Record.RecordID); err != nil {
		t.Fatal("cancel removed immutable orphan", err)
	}
	checkDecision(t, e, who, v.Ref, authz.PurposeGenerativeInput, errcode.UseRestricted)
}

type concurrentAssertionFiles struct {
	provenance.Files
	arrived chan struct{}
	release chan struct{}
}

func (f *concurrentAssertionFiles) AppendRecord(ctx context.Context, r storage.Record) (storage.RecordRef, error) {
	out, err := f.Files.AppendRecord(ctx, r)
	if err == nil && r.Kind == "rights_assertion" {
		f.arrived <- struct{}{}
		select {
		case <-f.release:
		case <-ctx.Done():
			return out, ctx.Err()
		}
	}
	return out, err
}
func TestM2RightsConcurrentReplayUsesOneAssertion(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	who := e.login().Context
	v := e.ingest(who, "concurrent-rights", []byte("synthetic concurrent target"), *rightsOwned())
	evidence := rightsEvidence(t, e, who, v, false)
	yes := true
	in := provenance.AssertionRequest{Subject: v.Ref, ManifestDigest: v.ManifestDigest, ExpectedRevision: 1, Kind: "restrict", Fields: provenance.AssertionFields{NoAI: &yes}, EvidenceIDs: []ids.ID{evidence.RecordID}, Reason: "synthetic concurrent restriction"}
	deps := e.rights.Deps
	files := &concurrentAssertionFiles{Files: deps.Files, arrived: make(chan struct{}, 2), release: make(chan struct{})}
	deps.Files = files
	service, err := provenance.New(deps)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		value provenance.AssertionReceipt
		err   error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			value, err := service.ApplyAssertion(ctx, who, "same-concurrent-assertion", in)
			results <- result{value, err}
		}()
	}
	for range 2 {
		select {
		case <-files.arrived:
		case <-time.After(10 * time.Second):
			close(files.release)
			t.Fatal("concurrent file writes did not arrive")
		}
	}
	close(files.release)
	a, b := <-results, <-results
	if a.err != nil || b.err != nil || a.value.RecordID != b.value.RecordID || a.value.OperationID != b.value.OperationID {
		t.Fatal(a, b)
	}
	inventory, err := service.RecoveryInventory(ctx)
	if err != nil || len(inventory.Open) != 0 || len(inventory.Records) != 2 || len(inventory.Findings) != 0 {
		t.Fatal(inventory, err)
	}
}
