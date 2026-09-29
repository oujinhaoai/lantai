package integration

import (
	"context"
	"encoding/json"
	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"os"
	"path/filepath"
	"testing"

	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/extensions"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/provenance"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// T05/T06 are explicit test fixtures, not simulated task execution claimed as
// production integration. Files, registry, identity, permissions and SQL are real.
type reviewExecutionFixture struct {
	failure error
	result  ledger.ReviewEvidenceInput
	actor   ids.ID
}

func (f *reviewExecutionFixture) Task(context.Context, authz.Context, commit.Committed, ledger.ReviewFlow, string) error {
	return f.failure
}
func (f *reviewExecutionFixture) VerifyEvidence(_ context.Context, _ authz.Context, _ commit.Committed, actor ids.ID, in ledger.ReviewEvidenceInput, _ bool) error {
	if f.failure != nil {
		return f.failure
	}
	a, _ := json.Marshal(in)
	b, _ := json.Marshal(f.result)
	if string(a) != string(b) || actor != f.actor {
		return errcode.New(errcode.PreconditionFailed, "execution result differs")
	}
	return nil
}
func TestM2ReviewEvidenceRealFilesAndCompletion(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	who := e.login().Context
	result := e.ingest(who, "evidence-record", []byte("synthetic evidence target"), *rightsOwned())
	v, err := e.ledger.Version(ctx, result.AssetID, result.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := extensions.New(extensions.Deps{DB: e.inst.DB(ownership.Main), Gate: e.inst.Gate(), ReleaseDigest: digest.Of([]byte("integration release"))})
	if err != nil {
		t.Fatal(err)
	}
	mctx, held, err := e.inst.Gate().Maintain(ctx, commands.ReasonStarting)
	if err != nil {
		t.Fatal(err)
	}
	err = registry.RegisterBuiltins(mctx)
	held.Release()
	e.inst.Gate().Open()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := e.storage.ReadManifest(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	check, err := registry.ValidateManifest(ctx, raw, v.Ref(who.InstanceID), v.ManifestDigest)
	if err != nil {
		t.Fatal(err)
	}
	input := ledger.ReviewEvidenceInput{ProjectID: v.ProjectID, Ref: v.Ref(who.InstanceID), ManifestDigest: v.ManifestDigest, Flow: ledger.ReviewFlow{TaskID: ids.New(), AttemptID: ids.New(), Round: 1, Fence: 1}, Kind: "check_result", CheckKey: "integrity", SchemaVersion: 1, ConfigDigest: digest.Of([]byte("config")), Check: &check}
	execution := &reviewExecutionFixture{result: input, actor: who.PrincipalID}
	source, err := e.ledger.NewFileReviewSources(e.storage, e.rights, execution, registry, e.rights)
	if err != nil {
		t.Fatal(err)
	}
	rightsRecord, err := e.rights.AppendEvidence(ctx, provenance.AppendRequest{Who: who, IdempotencyKey: e.key(), Ref: v.Ref(who.InstanceID), ManifestDigest: v.ManifestDigest, Evidence: provenance.Evidence{Note: "synthetic license evidence", ExternalInputs: []provenance.ExternalInput{}}})
	if err != nil {
		t.Fatal(err)
	}
	licenseEvidence, err := source.Evidence(ctx, who, rightsRecord.RecordID)
	if err != nil || licenseEvidence.Kind != "license_evidence" || licenseEvidence.ID != rightsRecord.RecordID || licenseEvidence.Flow.TaskID != "" {
		t.Fatal(licenseEvidence, err)
	}
	key := e.key()
	accepted, err := source.AppendEvidence(ctx, who, key, input)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := source.AppendEvidence(ctx, who, key, input)
	if err != nil || replay.ID != accepted.ID || replay.Digest != accepted.Digest {
		t.Fatal(replay, err)
	}
	read, err := source.Evidence(ctx, who, accepted.ID)
	if err != nil || read.Digest != accepted.Digest || read.ActorID != who.PrincipalID {
		t.Fatal(read, err)
	}
	wrong := input
	wrong.ConfigDigest = digest.Of([]byte("wrong"))
	if _, err = source.AppendEvidence(ctx, who, e.key(), wrong); errcode.CodeOf(err) != errcode.PreconditionFailed {
		t.Fatal(err)
	}
	// Disk files without a committed ledger acceptance do not resolve as evidence.
	orphan := storage.Record{RecordID: ids.New(), ProjectID: v.ProjectID, AssetID: v.AssetID, VersionID: v.VersionID, ManifestDigest: v.ManifestDigest, Kind: "check_result", PayloadSchema: "lantai.review-evidence/v1", AuthorID: who.PrincipalID, OperationID: ids.New(), CreatedAt: accepted.CompletedAt}
	orphan.Payload, _ = json.Marshal(input)
	if _, err = e.storage.AppendRecord(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	if _, err = source.Evidence(ctx, who, orphan.RecordID); errcode.CodeOf(err) != errcode.NotFound {
		t.Fatal("orphan evidence accepted", err)
	}
	execution.failure = errcode.New(errcode.LeaseStale, "")
	if _, err = source.AppendEvidence(ctx, who, e.key(), input); errcode.CodeOf(err) != errcode.LeaseStale {
		t.Fatal(err)
	}
	execution.failure = nil
	// Inject a commit failure after the file has been installed. Retry must retain
	// exactly the same file ID and finish the original operation, without double-counting.
	db := e.inst.DB(ownership.Ledger)
	if _, err = db.ExecContext(ctx, `CREATE TRIGGER reject_check BEFORE INSERT ON ledger_check_records BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`); err != nil {
		t.Fatal(err)
	}
	key = e.key()
	if _, err = source.AppendEvidence(ctx, who, key, input); err == nil {
		t.Fatal("fault unexpectedly committed")
	}
	var prepared string
	if err = db.QueryRowContext(ctx, `SELECT record_json FROM ledger_check_prepared`).Scan(&prepared); err != nil {
		t.Fatal(err)
	}
	var pending storage.Record
	if err = json.Unmarshal([]byte(prepared), &pending); err != nil {
		t.Fatal(err)
	}
	if _, err = source.Evidence(ctx, who, pending.RecordID); errcode.CodeOf(err) != errcode.NotFound {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `DROP TRIGGER reject_check`); err != nil {
		t.Fatal(err)
	}
	done, err := source.AppendEvidence(ctx, who, key, input)
	if err != nil || done.ID != pending.RecordID {
		t.Fatal(done, err)
	}
	var count int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM ledger_check_prepared`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	app := &application.App{Instance: e.inst, Ledger: e.ledger, Storage: e.storage, Catalog: e.catalog, Rights: e.rights}
	report, err := app.FSCK(ctx, true)
	if err != nil || report.Err() != nil || len(report.Ledger.Records) != 2 {
		t.Fatal(report.Findings, err)
	}
	for _, r := range report.Residuals {
		if r.OperationID == pending.OperationID {
			t.Fatal("accepted review evidence treated as orphan", r)
		}
	}
	assetDir := (storage.Layout{Home: e.inst.Layout().Home}).AssetDir(v.ProjectID, v.AssetID)
	path := filepath.Join(assetDir, "records", string(v.VersionID), string(accepted.ID)+".json")
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("corrupt accepted check"), 0600); err != nil {
		t.Fatal(err)
	}
	report, err = app.FSCK(ctx, true)
	if err != nil || report.Err() == nil {
		t.Fatal("corrupt accepted review evidence was not an integrity finding", report, err)
	}
}

// This adapter supplies a pre-approved profile and T05/T06 evidence fixture.
// The test specifically covers the actual identity/TOTP -> ledger boundary.
type humanReviewSources struct {
	e        *env
	profile  ledger.ProfileSnapshot
	evidence map[ids.ID]ledger.AcceptedEvidence
}

func (s *humanReviewSources) Profile(context.Context, authz.Context, ids.PermanentRef) (ledger.ProfileSnapshot, error) {
	return s.profile, nil
}
func (s *humanReviewSources) Evidence(_ context.Context, _ authz.Context, id ids.ID) (ledger.AcceptedEvidence, error) {
	v, ok := s.evidence[id]
	if !ok {
		return v, errcode.New(errcode.NotFound, "")
	}
	return v, nil
}
func (s *humanReviewSources) Task(context.Context, authz.Context, commit.Committed, ledger.ReviewFlow, string) error {
	return nil
}
func (s *humanReviewSources) Use(ctx context.Context, who authz.Context, v commit.Committed, purpose authz.Purpose) error {
	d, err := s.e.rights.EvaluateUse(ctx, who, v.Ref(who.InstanceID), purpose)
	if err != nil {
		return err
	}
	return d.Err()
}
func (s *humanReviewSources) AssetType(_ context.Context, v commit.Committed) (manifest.AssetType, error) {
	if v.VersionID == s.profile.Ref.VersionID {
		return manifest.TypeConfig, nil
	}
	return manifest.TypeDoc, nil
}
func TestM2ReviewHumanGrantBindsTargetAndRevoke(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	who := e.login().Context
	profile := e.ingest(who, "seed-profile", []byte("synthetic pre-approved profile fixture"), *rightsOwned())
	if _, err := e.inst.DB(ownership.Ledger).ExecContext(ctx, `UPDATE ledger_version_states SET state='approved',effective_review_id=? WHERE version_id=?`, ids.New(), profile.VersionID); err != nil {
		t.Fatal(err)
	}
	result := e.ingest(who, "human-review", []byte("synthetic reviewed content"), *rightsOwned())
	v, err := e.ledger.Version(ctx, result.AssetID, result.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	producer := storage.Producer{ExtensionID: "org.example.check", ExtensionVersion: "1.0.0", PackageDigest: digest.Of([]byte("fixture package")), ContributionID: "org.example.check.all", Source: "package"}
	config := digest.Of([]byte("config"))
	flow := ledger.ReviewFlow{TaskID: ids.New(), AttemptID: ids.New(), Round: 1, Fence: 1}
	p := ledger.AcceptanceProfile{Contract: "lantai.acceptance-profile/v1", ID: "synthetic", Revision: 1, AssetTypes: []manifest.AssetType{manifest.TypeDoc}, Purpose: authz.PurposeProduction, RequiredEvidence: []string{}, DistinctActorRule: "maker_checker", WaivableChecks: []string{}, Defaults: ledger.ProfileDefaults{Publication: "auto"}}
	source := &humanReviewSources{e: e, evidence: map[ids.ID]ledger.AcceptedEvidence{}}
	list := []ids.ID{}
	for _, key := range []string{"integrity", "schema", "license_evidence", "purpose"} {
		p.RequiredChecks = append(p.RequiredChecks, ledger.CheckRequirement{Key: key, SchemaVersion: 1, ConfigDigest: config, AcceptedProcessors: []storage.Producer{producer}, Severity: "error"})
		id := ids.New()
		list = append(list, id)
		source.evidence[id] = ledger.AcceptedEvidence{ID: id, Digest: digest.Of([]byte(key)), Ref: result.Ref, ManifestDigest: result.ManifestDigest, Kind: "check_result", ActorID: ids.New(), Flow: flow, CheckKey: key, SchemaVersion: 1, ConfigDigest: config, CompletedAt: clock.Format(e.clk.Now()), Check: &extensions.CheckResult{Contract: "lantai.check-result/v1", Ref: result.Ref, ManifestDigest: result.ManifestDigest, Verdict: "pass", Findings: []string{}, Producer: producer}}
	}
	source.profile = ledger.ProfileSnapshot{Ref: profile.Ref, ManifestDigest: profile.ManifestDigest, Profile: p}
	reviews, err := e.ledger.NewReviews(source, e.id)
	if err != nil {
		t.Fatal(err)
	}
	target, err := reviews.Submit(ctx, who, e.key(), ledger.SubmitReview{ProjectID: v.ProjectID, VersionID: v.VersionID, ExpectedRevision: 1, ProfileRef: profile.Ref, EvidenceIDs: list, Flow: flow})
	if err != nil {
		t.Fatal(err)
	}
	log, collaboration := collaborationForTest(t, e, reviews, nil, nil)
	e.relay(log)
	if _, err = collaboration.CatchUpInbox(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	inbox, err := collaboration.Inbox(ctx, who, v.ProjectID)
	if err != nil || len(inbox.Items) != 1 || inbox.Items[0].Object.Ref.ID != target.ID || inbox.Items[0].Object.State != "submitted" {
		t.Fatal(inbox, err)
	}
	if err = collaboration.MarkRead(ctx, who, v.ProjectID, inbox.Through); err != nil {
		t.Fatal(err)
	}
	authorize := func(inputs ...ledger.ReviewDecision) (ids.ID, ids.ID) {
		t.Helper()
		actions := []identity.HumanAction{}
		for _, in := range inputs {
			action, err := reviews.HumanAction(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			actions = append(actions, action)
		}
		ch, err := e.id.CreateDomainChallenge(ctx, who, actions, reviews)
		if err != nil {
			t.Fatal(err)
		}
		g, err := e.id.VerifyChallenge(ctx, who, ch.ChallengeID, e.fresh(), "192.0.2.10")
		if err != nil {
			t.Fatal(err)
		}
		items, err := e.id.DomainItems(ctx, who, g.GrantID)
		if err != nil {
			t.Fatal(err)
		}
		return g.GrantID, items[0].OperationID
	}
	decision := ledger.ReviewDecision{Action: identity.ActRecordReview, TargetID: target.ID, ExpectedRevision: target.Revision, Verdict: "approve", Reason: "Synthetic human approval", Waivers: []ledger.ReviewWaiver{}}
	other := e.ingest(who, "batch-review", []byte("second synthetic review target"), *rightsOwned())
	otherEvidence := []ids.ID{}
	for _, id := range list {
		evidence := source.evidence[id]
		evidence.ID, evidence.Ref, evidence.ManifestDigest = ids.New(), other.Ref, other.ManifestDigest
		check := *evidence.Check
		check.Ref, check.ManifestDigest = evidence.Ref, evidence.ManifestDigest
		evidence.Check = &check
		source.evidence[evidence.ID] = evidence
		otherEvidence = append(otherEvidence, evidence.ID)
	}
	otherTarget, err := reviews.Submit(ctx, who, e.key(), ledger.SubmitReview{ProjectID: v.ProjectID, VersionID: other.VersionID, ExpectedRevision: 1, ProfileRef: profile.Ref, EvidenceIDs: otherEvidence, Flow: flow})
	if err != nil {
		t.Fatal(err)
	}
	otherDecision := decision
	otherDecision.TargetID, otherDecision.ExpectedRevision = otherTarget.ID, otherTarget.Revision
	grant, child := authorize(decision, otherDecision)
	items, err := e.id.DomainItems(ctx, who, grant)
	if err != nil || len(items) != 2 {
		t.Fatal(items, err)
	}
	if _, err = reviews.Record(ctx, who, otherDecision, grant, child, e.id); errcode.CodeOf(err) != errcode.HumanGrantMismatch {
		t.Fatal("batch child accepted replacement target", err)
	}
	if _, err = reviews.Record(ctx, who, decision, grant, ids.New(), e.id); errcode.CodeOf(err) != errcode.HumanGrantMismatch {
		t.Fatal("batch accepted extra child", err)
	}
	secondReceipt, err := reviews.Record(ctx, who, otherDecision, grant, items[1].OperationID, e.id)
	if err != nil {
		t.Fatal(err)
	}
	if replay, err := reviews.Record(ctx, who, otherDecision, grant, items[1].OperationID, e.id); err != nil || replay.OperationID != secondReceipt.OperationID {
		t.Fatal("batch retry changed child result", replay, err)
	}
	changed := decision
	changed.Verdict = "reject"
	if _, err = reviews.Record(ctx, who, changed, grant, child, e.id); errcode.CodeOf(err) != errcode.HumanGrantMismatch {
		t.Fatal(err)
	}
	receipt, err := reviews.Record(ctx, who, decision, grant, child, e.id)
	if err != nil {
		t.Fatal(err)
	}
	// No relay after approval: old inbox candidates must disappear immediately.
	inbox, err = collaboration.Inbox(ctx, who, v.ProjectID)
	if err != nil || len(inbox.Items) != 0 {
		t.Fatal("approved review remains pending", inbox, err)
	}
	var review ledger.Review
	if err = json.Unmarshal(receipt.ResponseSummary, &review); err != nil {
		t.Fatal(err)
	}
	q, err := reviews.PublicationRequests(ctx, who, v.AssetID)
	if err != nil || len(q) != 1 {
		t.Fatal(q, err)
	}
	if _, err = reviews.RunPublication(ctx, who, q[0].ID); err != nil {
		t.Fatal(err)
	}
	revoke := ledger.ReviewDecision{Action: identity.ActRevokeReview, TargetID: target.ID, ExpectedRevision: 3, EffectiveReviewID: review.ID, Verdict: "revoke", Reason: "Correction of synthetic review", Waivers: []ledger.ReviewWaiver{}}
	rg, rc := authorize(revoke)
	if _, err = reviews.Record(ctx, who, revoke, rg, rc, e.id); err != nil {
		t.Fatal(err)
	}
	// Completed human action replay returns only its original immutable receipt.
	if replay, err := reviews.Record(ctx, who, decision, grant, child, e.id); err != nil || replay.OperationID != receipt.OperationID {
		t.Fatal(replay, err)
	}
	a, err := e.ledger.AssetControl(ctx, v.AssetID)
	if err != nil || a.PublicationState != "suspended" || a.PublishedVersionID != "" {
		t.Fatal(a, err)
	}
	state, err := e.ledger.VersionControl(ctx, v.VersionID)
	if err != nil || state.ReviewState != "withdrawn" {
		t.Fatal(state, err)
	}
}
