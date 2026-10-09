package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
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
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// Explicit T05/T06 completion fixture. Configuration, producer registration,
// evidence files, ledger approval and the human authorization are real modules.
type profileExecutionFixture struct {
	actor   ids.ID
	results map[string]ledger.ReviewEvidenceInput
}

func (f *profileExecutionFixture) Task(context.Context, authz.Context, commit.Committed, ledger.ReviewFlow, string) error {
	return nil
}
func (f *profileExecutionFixture) VerifyEvidence(_ context.Context, _ authz.Context, _ commit.Committed, actor ids.ID, in ledger.ReviewEvidenceInput, _ bool) error {
	a, _ := json.Marshal(in)
	b, _ := json.Marshal(f.results[string(in.Ref.VersionID)+":"+in.CheckKey])
	if actor != f.actor || string(a) != string(b) {
		return errcode.New(errcode.PreconditionFailed, "execution completion mismatch")
	}
	return nil
}
func TestM2InitialProfileRequiresOwnerEvidenceAndHumanApproval(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	ctx := t.Context()
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	who := e.login().Context
	registry, err := extensions.New(extensions.Deps{DB: e.inst.DB(ownership.Main), Gate: e.inst.Gate(), ReleaseDigest: digest.Of([]byte("profile test release"))})
	if err != nil {
		t.Fatal(err)
	}
	mctx, h, err := e.inst.Gate().Maintain(ctx, commands.ReasonStarting)
	if err != nil {
		t.Fatal(err)
	}
	err = registry.RegisterBuiltins(mctx)
	h.Release()
	e.inst.Gate().Open()
	if err != nil {
		t.Fatal(err)
	}
	producer, err := registry.Producer(ctx, "org.lantai.corecheck.manifest")
	if err != nil {
		t.Fatal(err)
	}
	config := digest.Of([]byte("fixed test checker configuration"))
	profile := ledger.AcceptanceProfile{Contract: "lantai.acceptance-profile/v1", ID: "project-base", Revision: 1, AssetTypes: []manifest.AssetType{manifest.TypeConfig, manifest.TypeDoc}, Purpose: authz.PurposeProduction, RequiredEvidence: []string{}, DistinctActorRule: "maker_checker", WaivableChecks: []string{}, Defaults: ledger.ProfileDefaults{Publication: "manual"}}
	for _, key := range []string{"integrity", "schema", "license_evidence", "purpose"} {
		profile.RequiredChecks = append(profile.RequiredChecks, ledger.CheckRequirement{Key: key, SchemaVersion: 1, AcceptedProcessors: []storage.Producer{producer}, ConfigDigest: config, Severity: "error"})
	}
	ingestProfile := func(slug string) catalog.VersionResult {
		t.Helper()
		data, _ := json.Marshal(profile)
		upload, err := e.storage.CreateUpload(ctx, storage.CreateUploadRequest{Who: who, IdempotencyKey: e.key(), ProjectID: e.project.ProjectID, Files: []storage.FileSpec{{SHA256: shaOf(data), Size: int64(len(data))}}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = e.storage.PutPart(ctx, storage.PartRequest{Who: who, UploadID: upload.UploadID, SHA256: shaOf(data), PartNumber: 1, PartSHA256: shaOf(data), Size: int64(len(data)), Body: bytes.NewReader(data)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = e.storage.CompleteFile(ctx, who, upload.UploadID, shaOf(data)); err != nil {
			t.Fatal(err)
		}
		var metadata any
		if err = json.Unmarshal(data, &metadata); err != nil {
			t.Fatal(err)
		}
		v, err := e.catalog.CommitVersion(ctx, catalog.VersionRequest{Who: who, IdempotencyKey: e.key(), UploadID: upload.UploadID, Slug: slug, Content: catalog.ContentInput{AssetType: manifest.TypeConfig, Rights: rightsOwned(), Files: []manifest.InputFile{{Path: "profile.json", Role: "primary", SHA256: shaOf(data), Size: int64(len(data))}}, Metadata: map[string]any{"acceptance_profile": metadata}}})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	first := ingestProfile("initial-profile")
	execution := &profileExecutionFixture{actor: who.PrincipalID, results: map[string]ledger.ReviewEvidenceInput{}}
	source, err := e.ledger.NewFileReviewSources(e.storage, e.rights, execution, registry, e.rights)
	if err != nil {
		t.Fatal(err)
	}
	reviews, err := e.ledger.NewReviews(source, e.id)
	if err != nil {
		t.Fatal(err)
	}
	flow := ledger.ReviewFlow{TaskID: ids.New(), AttemptID: ids.New(), Round: 1, Fence: 1}
	checks := func(result catalog.VersionResult) []ids.ID {
		t.Helper()
		v, err := e.ledger.Version(ctx, result.AssetID, result.VersionID)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := e.storage.ReadManifest(ctx, v)
		if err != nil {
			t.Fatal(err)
		}
		checked, err := registry.ValidateManifest(ctx, raw, result.Ref, result.ManifestDigest)
		if err != nil {
			t.Fatal(err)
		}
		var list []ids.ID
		for _, req := range profile.RequiredChecks {
			input := ledger.ReviewEvidenceInput{ProjectID: e.project.ProjectID, Ref: result.Ref, ManifestDigest: result.ManifestDigest, Flow: flow, Kind: "check_result", CheckKey: req.Key, SchemaVersion: 1, ConfigDigest: config, Check: &checked}
			execution.results[string(result.VersionID)+":"+req.Key] = input
			evidence, err := source.AppendEvidence(ctx, who, e.key(), input)
			if err != nil {
				t.Fatal(err)
			}
			list = append(list, evidence.ID)
		}
		return list
	}
	input := ledger.SubmitReview{ProjectID: e.project.ProjectID, VersionID: first.VersionID, ExpectedRevision: 1, ProfileRef: first.Ref, Flow: flow, EvidenceIDs: checks(first)}
	if _, err = reviews.Submit(ctx, who, e.key(), input); errcode.CodeOf(err) != errcode.ChecksNotSatisfied {
		t.Fatal("unapproved profile accepted normally", err)
	}
	input.InitialProfile = true
	_, contributor := e.agent("profile-contributor@node", identity.RoleContributor)
	if _, err = reviews.Submit(ctx, contributor.Context, e.key(), input); errcode.CodeOf(err) != errcode.Forbidden {
		t.Fatal("non-owner initialized profile", err)
	}
	target, err := reviews.Submit(ctx, who, e.key(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !target.InitialProfile {
		t.Fatal(target)
	}
	state, err := e.ledger.VersionControl(ctx, first.VersionID)
	if err != nil || state.ReviewState != "submitted" {
		t.Fatal(state, err)
	}
	// A second candidate can exist before any approval; only the final current
	// authority may decide which one initializes the project.
	competing := ingestProfile("competing-initial-profile")
	competingInput := ledger.SubmitReview{InitialProfile: true, ProjectID: e.project.ProjectID, VersionID: competing.VersionID, ExpectedRevision: 1, ProfileRef: competing.Ref, Flow: flow, EvidenceIDs: checks(competing)}
	competingTarget, err := reviews.Submit(ctx, who, e.key(), competingInput)
	if err != nil {
		t.Fatal(err)
	}
	decision := ledger.ReviewDecision{Action: identity.ActRecordReview, TargetID: target.ID, ExpectedRevision: target.Revision, Verdict: "approve", Reason: "initialize exact project profile after checks", Waivers: []ledger.ReviewWaiver{}}
	action, err := reviews.HumanAction(ctx, decision)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := e.id.CreateDomainChallenge(ctx, who, []identity.HumanAction{action}, reviews)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := e.id.VerifyChallenge(ctx, who, challenge.ChallengeID, e.fresh(), "192.0.2.10")
	if err != nil {
		t.Fatal(err)
	}
	items, err := e.id.DomainItems(ctx, who, grant.GrantID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reviews.Record(ctx, who, decision, grant.GrantID, items[0].OperationID, e.id); err != nil {
		t.Fatal(err)
	}
	state, err = e.ledger.VersionControl(ctx, first.VersionID)
	if err != nil || state.ReviewState != "approved" {
		t.Fatal(state, err)
	}
	competingDecision := ledger.ReviewDecision{Action: identity.ActRecordReview, TargetID: competingTarget.ID, ExpectedRevision: competingTarget.Revision, Verdict: "approve", Reason: "must recheck initialization eligibility", Waivers: []ledger.ReviewWaiver{}}
	competingAction, err := reviews.HumanAction(ctx, competingDecision)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err = e.id.CreateDomainChallenge(ctx, who, []identity.HumanAction{competingAction}, reviews)
	if err != nil {
		t.Fatal(err)
	}
	grant, err = e.id.VerifyChallenge(ctx, who, challenge.ChallengeID, e.fresh(), "192.0.2.10")
	if err != nil {
		t.Fatal(err)
	}
	items, err = e.id.DomainItems(ctx, who, grant.GrantID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reviews.Record(ctx, who, competingDecision, grant.GrantID, items[0].OperationID, e.id); errcode.CodeOf(err) != errcode.InvalidStateTransition {
		t.Fatal("staged initialization bypassed newer approval", err)
	}
	state, err = e.ledger.VersionControl(ctx, competing.VersionID)
	if err != nil || state.ReviewState != "submitted" {
		t.Fatal(state, err)
	}
	second := ingestProfile("ordinary-next-profile")
	next := ledger.SubmitReview{InitialProfile: true, ProjectID: e.project.ProjectID, VersionID: second.VersionID, ExpectedRevision: 1, ProfileRef: second.Ref, Flow: flow, EvidenceIDs: checks(second)}
	if _, err = reviews.Submit(ctx, who, e.key(), next); errcode.CodeOf(err) != errcode.InvalidStateTransition {
		t.Fatal("initialization bypassed existing approved configuration", err)
	}
	next.InitialProfile = false
	next.ProfileRef = first.Ref
	if _, err = reviews.Submit(ctx, who, e.key(), next); err != nil {
		t.Fatal("approved initial profile cannot govern normal submissions", err)
	}
}

// CheckApplicable is a completion fixture; real activation acceptance is tested
// through the assembled application and jobs service in m2_evidence_governance.
func (f *profileExecutionFixture) CheckApplicable(ctx context.Context, who authz.Context, v commit.Committed, actor ids.ID, in ledger.ReviewEvidenceInput, _ ids.ID) error {
	return f.VerifyEvidence(ctx, who, v, actor, in, true)
}
