package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/extensions"
	"github.com/oujinhaoai/lantai/internal/extensions/exttest"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/jobs"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/node"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/tasks"
	"github.com/oujinhaoai/lantai/internal/workflow"
)

// These helpers use assembled owners and real processes. They never write
// approval, enablement, task completion or check evidence through SQL.
func (f *appFlow) candidate(slug string, typ manifest.AssetType, files map[string][]byte, metadata map[string]any) (catalog.VersionResult, ledger.ReviewFlow) {
	return f.candidateVersion(slug, typ, files, metadata, nil)
}
func (f *appFlow) candidateVersion(slug string, typ manifest.AssetType, files map[string][]byte, metadata map[string]any, previous *catalog.VersionResult) (catalog.VersionResult, ledger.ReviewFlow) {
	f.t.Helper()
	ctx := f.t.Context()
	c, err := f.tasks.Create(ctx, f.owner, f.key(), tasks.CreateRequest{ProjectID: f.project.ProjectID, Type: "produce", Title: slug, AcceptanceCriteria: []string{"synthetic"}, Role: identity.RoleContributor, ExpectedOutputs: []tc.OutputRequirement{{Slug: "out", AssetType: string(typ), CandidateCount: 1}}})
	if err != nil {
		f.t.Fatal(err)
	}
	a := f.claim(f.maker, c.TaskID)
	upload, inputs := f.uploadFiles(f.maker, files)
	req := catalog.VersionRequest{Who: f.maker, IdempotencyKey: f.key(), UploadID: upload, Slug: slug, Task: bindTo(c.TaskID, a), Content: catalog.ContentInput{AssetType: typ, Rights: rightsOwned(), Files: inputs, Metadata: metadata}}
	if previous != nil {
		req.Slug = ""
		req.AssetID = previous.AssetID
		req.BaseVersionID = previous.VersionID
	}
	v, err := f.catalog.CommitVersion(ctx, req)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err = f.tasks.Submit(ctx, f.maker, f.key(), tasks.SubmitRequest{AttemptRef: attemptRef(f.flowEnv, c.TaskID, a), OutputRefs: []ids.PermanentRef{v.Ref}}); err != nil {
		f.t.Fatal(err)
	}
	return v, ledger.ReviewFlow{TaskID: c.TaskID, AttemptID: a.ID, Round: 1, Fence: a.Fence.LeaseFence}
}
func (f *appFlow) checkCandidate(v catalog.VersionResult, flow ledger.ReviewFlow, processor string) jobs.Job {
	f.t.Helper()
	ctx := f.t.Context()
	ref, err := f.app.Jobs.StartJob(ctx, f.owner, workflow.JobRequest{OperationID: ids.New(), ProjectID: f.project.ProjectID, Processor: processor, Target: v.Ref, Flow: flow, ManifestDigest: string(v.ManifestDigest)})
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err = f.app.Nodes.Observe(ctx, f.worker, f.key(), node.Observation{ProjectID: f.project.ProjectID, Capabilities: []string{processor}, Slots: 1}); err != nil {
		f.t.Fatal(err)
	}
	j, err := f.app.Jobs.Read(ctx, f.owner, ref.JobID)
	if err != nil {
		f.t.Fatal(err)
	}
	j, err = f.app.Jobs.Run(ctx, f.worker, f.key(), jobs.Control{JobID: j.ID, ExpectedRevision: j.Revision})
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Logf("job processor=%s state=%s failure=%s checks=%d", processor, j.State, j.Failure, len(j.Checks))
	return j
}
func evidenceIDs(j jobs.Job) []ids.ID {
	out := []ids.ID{}
	for _, e := range j.Evidence {
		out = append(out, e.EvidenceID)
	}
	return out
}
func (f *appFlow) submitChecked(v catalog.VersionResult, flow ledger.ReviewFlow, profile ids.PermanentRef, j jobs.Job, initial bool) ledger.ReviewTarget {
	f.t.Helper()
	s, err := f.ledger.VersionControl(f.t.Context(), v.VersionID)
	if err != nil {
		f.t.Fatal(err)
	}
	target, err := f.reviews.Submit(f.t.Context(), f.owner, f.key(), ledger.SubmitReview{ProjectID: f.project.ProjectID, VersionID: v.VersionID, ExpectedRevision: s.Revision, ProfileRef: profile, Flow: flow, EvidenceIDs: evidenceIDs(j), InitialProfile: initial})
	if err != nil {
		f.t.Fatal(err)
	}
	return target
}
func profileDocument(id string, producer storage.Producer) ledger.AcceptanceProfile {
	p := ledger.AcceptanceProfile{Contract: "lantai.acceptance-profile/v1", ID: id, Revision: 1, AssetTypes: []manifest.AssetType{manifest.TypeConfig, manifest.TypePlugin, manifest.TypeDoc}, Purpose: authz.PurposeProduction, RequiredEvidence: []string{}, DistinctActorRule: "maker_checker", WaivableChecks: []string{}, Defaults: ledger.ProfileDefaults{Publication: "manual"}}
	for _, key := range []string{"integrity", "schema", "license_evidence", "purpose"} {
		p.RequiredChecks = append(p.RequiredChecks, ledger.CheckRequirement{Key: key, SchemaVersion: 1, AcceptedProcessors: []storage.Producer{producer}, ConfigDigest: jobs.ConfigDigest(), Severity: "error"})
	}
	return p
}
func (f *appFlow) approvedProfile(slug string, p ledger.AcceptanceProfile, under ids.PermanentRef, previous ...catalog.VersionResult) catalog.VersionResult {
	f.t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		f.t.Fatal(err)
	}
	var doc any
	if err = json.Unmarshal(raw, &doc); err != nil {
		f.t.Fatal(err)
	}
	var prior *catalog.VersionResult
	if len(previous) > 0 {
		prior = &previous[0]
	}
	v, flow := f.candidateVersion(slug, manifest.TypeConfig, map[string][]byte{"profile.json": raw}, map[string]any{"acceptance_profile": doc}, prior)
	j := f.checkCandidate(v, flow, "org.lantai.corecheck.manifest")
	initial := under.VersionID == ""
	if initial {
		under = v.Ref
	}
	target := f.submitChecked(v, flow, under, j, initial)
	f.decide(target.ID, "approve")
	return v
}
func (f *appFlow) approvedPackage(base catalog.VersionResult, slug, version string) (extensions.Package, extensions.PackageRecord) {
	f.t.Helper()
	dir := f.t.TempDir()
	pkg := exttest.Write(f.t, dir, exttest.Spec{ID: "org.example.business", Version: version, Server: true, CLI: true})
	v, flow := f.candidate(slug, manifest.TypePlugin, readDir(f.t, dir), map[string]any{"extension_id": pkg.Manifest.ID, "extension_version": version})
	j := f.checkCandidate(v, flow, "org.lantai.corecheck.manifest")
	target := f.submitChecked(v, flow, base.Ref, j, false)
	f.decide(target.ID, "approve")
	rec, err := f.app.ExtensionManager.Import(f.t.Context(), f.login().Context, f.key(), extensions.ImportRequest{AssetID: v.AssetID, VersionID: v.VersionID})
	if err != nil {
		f.t.Fatal(err)
	}
	return pkg, rec
}
func (f *appFlow) enablePackage(pkg extensions.Package, config string, revision int64) extensions.ChangeResult {
	f.t.Helper()
	req := extensions.EnableRequest{ExtensionID: pkg.Manifest.ID, ExtensionVersion: pkg.Manifest.Version, PackageDigest: pkg.Digest, Target: "server", ScopeKind: "instance", Config: json.RawMessage(config), ConfigRevision: revision, Trust: extensions.TrustUnenforced, Probe: true, Reason: "synthetic business checker"}
	a, err := f.app.ExtensionManager.EnableHumanAction(f.t.Context(), req)
	if err != nil {
		f.t.Fatal(err)
	}
	who, g, op := f.grant(a)
	result, err := f.app.ExtensionManager.Enable(f.t.Context(), who, req, g, op, f.id)
	if err != nil {
		activation, _ := json.Marshal(result.Activation)
		f.t.Fatalf("enablement probe: %v; activation=%s", err, activation)
	}
	return result
}
func TestM2BusinessCheckRealJob(t *testing.T) {
	f := newAppFlow(t)
	builtin, err := f.app.Extensions.Producer(t.Context(), "org.lantai.corecheck.manifest")
	if err != nil {
		t.Fatal(err)
	}
	base := f.approvedProfile("profiles/bootstrap", profileDocument("bootstrap", builtin), ids.PermanentRef{})
	pkg, _ := f.approvedPackage(base, "plugins/business-v1", "0.1.0")
	f.enablePackage(pkg, `{"mode":"business"}`, 1)
	f.sudo(&identity.SetPolicy{ProjectID: f.project.ProjectID, Key: "plugins.allowed", Value: json.RawMessage(`["org.example.business"]`)})
	v, flow := f.candidate("docs/business", manifest.TypeDoc, map[string][]byte{"data.json": []byte(`{"valid":true}`)}, map[string]any{"synthetic_valid": true})
	j := f.checkCandidate(v, flow, pkg.Manifest.ID+".check")
	if j.State != "succeeded" || len(j.Checks) != 5 || j.Checks[4].CheckKey != "media_structure" || j.Checks[4].Check.Verdict != execution.VerdictPass {
		t.Fatalf("business evidence was not accepted: state=%s failure=%s checks=%d", j.State, j.Failure, len(j.Checks))
	}
}

func (f *appFlow) reviewDecision(target ledger.ReviewTarget, waivers []ledger.ReviewWaiver) ledger.ReviewDecision {
	return ledger.ReviewDecision{Action: identity.ActRecordReview, TargetID: target.ID, ExpectedRevision: target.Revision, Verdict: "approve", Reason: "responsible synthetic acceptance", Waivers: waivers}
}
func (f *appFlow) recordDecision(d ledger.ReviewDecision) (ledger.Review, error) {
	f.t.Helper()
	a, err := f.reviews.HumanAction(f.t.Context(), d)
	if err != nil {
		return ledger.Review{}, err
	}
	who, g, op := f.grant(a)
	receipt, err := f.reviews.Record(f.t.Context(), who, d, g, op, f.id)
	var review ledger.Review
	if err == nil {
		err = json.Unmarshal(receipt.ResponseSummary, &review)
	}
	return review, err
}
func businessProfile(id string, producer storage.Producer, config digest.Digest) ledger.AcceptanceProfile {
	p := profileDocument(id, producer)
	p.WaivableChecks = []string{"media_structure"}
	p.RequiredChecks = append(p.RequiredChecks, ledger.CheckRequirement{Key: "media_structure", SchemaVersion: 1, AcceptedProcessors: []storage.Producer{producer}, ConfigDigest: config, Severity: "error", Waivable: true})
	return p
}
func TestM2BusinessWaiversAndUpgrade(t *testing.T) {
	f := newAppFlow(t)
	ctx := t.Context()
	m := f.app.ExtensionManager
	builtin, err := f.app.Extensions.Producer(ctx, "org.lantai.corecheck.manifest")
	if err != nil {
		t.Fatal(err)
	}
	base := f.approvedProfile("profiles/bootstrap", profileDocument("bootstrap", builtin), ids.PermanentRef{})
	pkg1, rec1 := f.approvedPackage(base, "plugins/business-v1", "0.1.0")
	enabled1 := f.enablePackage(pkg1, `{"mode":"business"}`, 1)
	f.sudo(&identity.SetPolicy{ProjectID: f.project.ProjectID, Key: "plugins.allowed", Value: json.RawMessage(`["org.example.business"]`)})
	producer1 := storage.Producer{ExtensionID: pkg1.Manifest.ID, ExtensionVersion: pkg1.Manifest.Version, PackageDigest: pkg1.Digest, Source: "package", ContributionID: pkg1.Manifest.ID + ".check"}
	p1 := businessProfile("media", producer1, enabled1.Enablement.ConfigDigest)
	profile1 := f.approvedProfile("profiles/media-v1", p1, base.Ref)
	// A real legal failing verdict is persisted as evidence and does not trip the breaker.
	var retained jobs.Job
	for _, kind := range []string{"no_waiver", "expired", "empty_reason", "foreign_evidence", "core_integrity", "core_schema", "core_license_evidence", "core_purpose", "missing_result", "unauthorized", "wrong_target", "revoked_role", "valid"} {
		t.Run(kind, func(t *testing.T) {
			v, flow := f.candidate("docs/"+kind, manifest.TypeDoc, map[string][]byte{"data.json": []byte(`{"valid":false}`)}, map[string]any{"synthetic_valid": false})
			j := f.checkCandidate(v, flow, producer1.ContributionID)
			if j.State != "succeeded" || j.Attempt.Outcome != execution.InvocationCompleted || j.Attempt.Verdict != execution.VerdictFail || j.Checks[4].Check.Verdict != execution.VerdictFail {
				t.Fatalf("legal fail became runtime fault: %+v", j)
			}
			for _, x := range j.Checks[:4] {
				if x.Check.Verdict != execution.VerdictPass {
					t.Fatal("business fail contaminated core", x)
				}
			}
			if kind == "missing_result" {
				j.Evidence = j.Evidence[:4]
			}
			target := f.submitChecked(v, flow, profile1.Ref, j, false)
			waivers := []ledger.ReviewWaiver{{CheckKey: "media_structure", Reason: "responsible acceptance of synthetic structure failure", EvidenceIDs: []ids.ID{j.Evidence[0].EvidenceID}, ExpiresAt: clock.Format(f.clk.Now().Add(24 * time.Hour))}}
			switch kind {
			case "no_waiver":
				waivers = []ledger.ReviewWaiver{}
			case "expired":
				waivers[0].ExpiresAt = clock.Format(f.clk.Now())
			case "empty_reason":
				waivers[0].Reason = ""
			case "foreign_evidence":
				waivers[0].EvidenceIDs = []ids.ID{ids.New()}
			case "core_integrity", "core_schema", "core_license_evidence", "core_purpose":
				waivers[0].CheckKey = strings.TrimPrefix(kind, "core_")
			}
			d := f.reviewDecision(target, waivers)
			if kind == "unauthorized" || kind == "wrong_target" || kind == "revoked_role" {
				a, err := f.reviews.HumanAction(ctx, d)
				if err != nil {
					t.Fatal(err)
				}
				who, g, op := f.grant(a)
				switch kind {
				case "unauthorized":
					who = f.maker
				case "wrong_target":
					d.TargetID = ids.New()
				case "revoked_role":
					f.setRole(who.PrincipalID, identity.RoleOwner, false)
				}
				_, err = f.reviews.Record(ctx, who, d, g, op, f.id)
				if kind == "revoked_role" {
					f.setRole(who.PrincipalID, identity.RoleOwner, true)
				}
				if err == nil {
					t.Fatal("invalid authorization accepted")
				}
				t.Log("refusal", errcode.CodeOf(err))
				return
			}
			review, err := f.recordDecision(d)
			if kind != "valid" {
				if errcode.CodeOf(err) != errcode.ChecksNotSatisfied {
					t.Fatal("expected rejection", err)
				}
				t.Log("refusal", errcode.CodeOf(err))
				return
			}
			if err != nil || len(review.WaiverRecords) != 1 {
				t.Fatal(review, err)
			}
			w := review.WaiverRecords[0]
			if w.VersionID != v.VersionID || w.ManifestDigest != v.ManifestDigest || w.TargetID != target.ID || w.ApproverID != f.owner.PrincipalID || w.Reason == "" || w.HumanGrantID == "" || w.Scope != "review_target" {
				t.Fatal(w)
			}
			retained = j
			t.Logf("waiver=%s review=%s target=%s manifest=%s", w.ID, review.ID, target.ID, v.ManifestDigest)
		})
	}
	views, err := m.Enablements(ctx, f.login().Context)
	if err != nil || views[0].Breaker != nil && views[0].Breaker.State != "closed" {
		t.Fatal(views, err)
	}
	// Exercise binding before submission, so an immutable-target rejection
	// cannot mask the actual provenance/identity validation.
	bindingVersion, bindingFlow := f.candidate("docs/binding", manifest.TypeDoc, map[string][]byte{"data.json": []byte(`{"valid":true}`)}, map[string]any{"synthetic_valid": true})
	retained = f.checkCandidate(bindingVersion, bindingFlow, producer1.ContributionID)
	for _, kind := range []string{"config", "plugin", "manifest", "run", "job_run", "target", "key"} {
		in := retained.Checks[4]
		check := *in.Check
		in.Check = &check
		switch kind {
		case "config":
			in.ConfigDigest = digest.Of([]byte("wrong"))
		case "plugin":
			check.Producer.ExtensionVersion = "9.9.9"
		case "manifest":
			in.ManifestDigest = digest.Of([]byte("wrong"))
			check.ManifestDigest = in.ManifestDigest
		case "run":
			in.Flow.AttemptID = ids.New()
		case "job_run":
			in.CheckRunID = ids.New()
		case "target":
			in.Ref.VersionID = ids.New()
			check.Ref = in.Ref
		case "key":
			in.CheckKey = "other"
		}
		if _, err = f.source.AppendEvidence(ctx, f.worker, f.key(), in); err == nil {
			t.Fatal("replacement accepted", kind)
		}
		if errcode.CodeOf(err) == errcode.ReviewTargetStale {
			t.Fatal("binding test masked by submitted target state", kind, err)
		}
		t.Log("replacement", kind, errcode.CodeOf(err))
	}
	// Keep one fixed target awaiting review throughout a real package/Profile upgrade.
	v, flow := f.candidate("docs/upgrade", manifest.TypeDoc, map[string][]byte{"data.json": []byte(`{"valid":true}`)}, map[string]any{"synthetic_valid": true})
	old := f.checkCandidate(v, flow, producer1.ContributionID)
	before := map[ids.ID]ledger.AcceptedEvidence{}
	beforeBytes := map[ids.ID]digest.Digest{}
	for _, id := range evidenceIDs(old) {
		e, err := f.source.Evidence(ctx, f.owner, id)
		if err != nil {
			t.Fatal(err)
		}
		before[id] = e
		raw, err := f.app.Storage.ReadRecord(ctx, f.project.ProjectID, v.AssetID, v.VersionID, id)
		if err != nil {
			t.Fatal(err)
		}
		beforeBytes[id] = digest.Of(raw)
		captureGovernanceEvidence(t, "check-before-"+string(id)+".json", raw)
		t.Log("immutable check bytes before", id, beforeBytes[id])
	}
	profileState, err := f.ledger.VersionControl(ctx, profile1.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	var originalReview string
	if err = f.inst.DB(ownership.Ledger).QueryRowContext(ctx, `SELECT record FROM ledger_reviews WHERE review_id=?`, profileState.EffectiveReviewID).Scan(&originalReview); err != nil {
		t.Fatal(err)
	}
	captureGovernanceEvidence(t, "profile-review-before.json", []byte(originalReview))
	staleVersion, staleFlow := f.candidate("docs/stale-only", manifest.TypeDoc, map[string][]byte{"data.json": []byte(`{"valid":true}`)}, map[string]any{"synthetic_valid": true})
	staleChecks := f.checkCandidate(staleVersion, staleFlow, producer1.ContributionID)
	pkg2, rec2 := f.approvedPackage(base, "plugins/business-v2", "0.2.0")
	if rec1.PackageDigest == rec2.PackageDigest || rec1.Ref == rec2.Ref {
		t.Fatal("upgrade was not a new reviewed package")
	}
	enabled2 := f.enablePackage(pkg2, `{"mode":"business"}`, 1)
	producer2 := producer1
	producer2.ExtensionVersion = pkg2.Manifest.Version
	producer2.PackageDigest = pkg2.Digest
	p2 := businessProfile("media", producer2, enabled2.Enablement.ConfigDigest)
	p2.Revision = 2
	profile2 := f.approvedProfile("profiles/media-v2", p2, base.Ref, profile1)
	target := f.submitChecked(staleVersion, staleFlow, profile2.Ref, staleChecks, false)
	if _, err = f.recordDecision(f.reviewDecision(target, []ledger.ReviewWaiver{})); errcode.CodeOf(err) != errcode.ExtensionActivationStale {
		t.Fatal("retired plugin generation accepted", err)
	}
	t.Log("applicability stale: old plugin generation has retired")
	// Keep the Profile mismatch assertion independent of activation retirement:
	// a currently qualified run still cannot satisfy the old package requirement.
	profileVersion, profileFlow := f.candidate("docs/profile-mismatch", manifest.TypeDoc, map[string][]byte{"data.json": []byte(`{"valid":true}`)}, map[string]any{"synthetic_valid": true})
	profileChecks := f.checkCandidate(profileVersion, profileFlow, producer2.ContributionID)
	target = f.submitChecked(profileVersion, profileFlow, profile1.Ref, profileChecks, false)
	if _, err = f.recordDecision(f.reviewDecision(target, []ledger.ReviewWaiver{})); errcode.CodeOf(err) != errcode.ChecksNotSatisfied {
		t.Fatal("current plugin satisfied old Profile", err)
	}
	t.Log("applicability stale: current processor package/version differs from requested Profile")
	// Review targets freeze their selected evidence. A separate candidate
	// exercises unknown; the fixed upgrade candidate receives the new run.
	unknownVersion, unknownFlow := f.candidate("docs/unknown", manifest.TypeDoc, map[string][]byte{"data.json": []byte(`{"valid":true}`)}, map[string]any{"synthetic_valid": true})
	incomplete := f.checkCandidate(unknownVersion, unknownFlow, producer2.ContributionID)
	incomplete.Evidence = incomplete.Evidence[:4]
	target = f.submitChecked(unknownVersion, unknownFlow, profile2.Ref, incomplete, false)
	if _, err = f.recordDecision(f.reviewDecision(target, []ledger.ReviewWaiver{})); errcode.CodeOf(err) != errcode.ChecksNotSatisfied {
		t.Fatal("unknown check accepted", err)
	}
	t.Log("applicability unknown: required media_structure absent")
	fresh := f.checkCandidate(v, flow, producer2.ContributionID)
	target = f.submitChecked(v, flow, profile2.Ref, fresh, false)
	if _, err = f.recordDecision(f.reviewDecision(target, []ledger.ReviewWaiver{})); err != nil {
		t.Fatal("new check rejected", err)
	}
	if fresh.Attempt.ID == old.Attempt.ID || fresh.Checks[4].Check.Producer != producer2 || fresh.Checks[4].ConfigDigest != enabled2.Enablement.ConfigDigest {
		t.Fatal("new binding differs", fresh)
	}
	for id, want := range before {
		got, err := f.source.Evidence(ctx, f.owner, id)
		if err != nil || rawJSON(got) != rawJSON(want) {
			t.Fatal("history changed", id, err)
		}
		raw, err := f.app.Storage.ReadRecord(ctx, f.project.ProjectID, v.AssetID, v.VersionID, id)
		if err != nil || digest.Of(raw) != beforeBytes[id] {
			t.Fatal("immutable check bytes changed", id, err)
		}
		t.Log("immutable check bytes after", id, digest.Of(raw))
		captureGovernanceEvidence(t, "check-after-"+string(id)+".json", raw)
	}
	var retainedReview string
	if err = f.inst.DB(ownership.Ledger).QueryRowContext(ctx, `SELECT record FROM ledger_reviews WHERE review_id=?`, profileState.EffectiveReviewID).Scan(&retainedReview); err != nil || retainedReview != originalReview {
		t.Fatal("immutable Review changed", err)
	}
	captureGovernanceEvidence(t, "profile-review-after.json", []byte(retainedReview))
	t.Logf("upgrade old_package=%s new_package=%s old_profile=%s new_profile=%s old_job=%s new_job=%s", pkg1.Digest, pkg2.Digest, profile1.ManifestDigest, profile2.ManifestDigest, old.ID, fresh.ID)
}
func rawJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestM2BusinessEvidenceRevokedBeforeReview(t *testing.T) {
	for _, kind := range []string{"enablement", "package_review", "project_allowlist"} {
		t.Run(kind, func(t *testing.T) { testBusinessEvidenceRevokedBeforeReview(t, kind) })
	}
}
func testBusinessEvidenceRevokedBeforeReview(t *testing.T, kind string) {
	f := newAppFlow(t)
	ctx := t.Context()
	builtin, err := f.app.Extensions.Producer(ctx, "org.lantai.corecheck.manifest")
	if err != nil {
		t.Fatal(err)
	}
	base := f.approvedProfile("profiles/bootstrap", profileDocument("bootstrap", builtin), ids.PermanentRef{})
	pkg, rec := f.approvedPackage(base, "plugins/business", "0.1.0")
	enabled := f.enablePackage(pkg, `{"mode":"business"}`, 1)
	f.sudo(&identity.SetPolicy{ProjectID: f.project.ProjectID, Key: "plugins.allowed", Value: json.RawMessage(`["org.example.business"]`)})
	producer := storage.Producer{ExtensionID: pkg.Manifest.ID, ExtensionVersion: pkg.Manifest.Version, PackageDigest: pkg.Digest, Source: "package", ContributionID: pkg.Manifest.ID + ".check"}
	profile := f.approvedProfile("profiles/media", businessProfile("media", producer, enabled.Enablement.ConfigDigest), base.Ref)
	v, flow := f.candidate("docs/revoked-review", manifest.TypeDoc, map[string][]byte{"data.json": []byte(`{"valid":true}`)}, map[string]any{"synthetic_valid": true})
	j := f.checkCandidate(v, flow, producer.ContributionID)
	target := f.submitChecked(v, flow, profile.Ref, j, false)
	d := f.reviewDecision(target, []ledger.ReviewWaiver{})
	action, err := f.reviews.HumanAction(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	who, g, op := f.grant(action)
	switch kind {
	case "enablement":
		request := extensions.DisableRequest{EnablementID: enabled.Enablement.ID, Mode: "revoke", Reason: "security revocation before final review"}
		action, err = f.app.ExtensionManager.DisableHumanAction(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		admin, grant, child := f.grant(action)
		if _, err = f.app.ExtensionManager.Disable(ctx, admin, request, grant, child, f.id); err != nil {
			t.Fatal(err)
		}
	case "package_review":
		state, e := f.ledger.VersionControl(ctx, rec.Ref.VersionID)
		if e != nil {
			t.Fatal(e)
		}
		_, err = f.recordDecision(ledger.ReviewDecision{Action: identity.ActRevokeReview, TargetID: state.ReviewTargetID, ExpectedRevision: state.Revision, EffectiveReviewID: state.EffectiveReviewID, Verdict: "revoke", Reason: "withdraw reviewed synthetic package"})
		if err != nil {
			t.Fatal(err)
		}
	case "project_allowlist":
		f.sudo(&identity.SetPolicy{ProjectID: f.project.ProjectID, Key: "plugins.allowed", Value: json.RawMessage(`[]`), ExpectedRevision: 1})
	}
	if _, err = f.reviews.Record(ctx, who, d, g, op, f.id); err == nil {
		t.Fatal("revoked activation approved by final human acceptance")
	}
	want := errcode.ExtensionActivationStale
	if kind == "project_allowlist" {
		want = errcode.Forbidden
	}
	if errcode.CodeOf(err) != want {
		t.Fatal("unexpected final refusal", kind, err)
	}
	state, e := f.ledger.VersionControl(ctx, v.VersionID)
	if e != nil || state.ReviewState != "submitted" || state.EffectiveReviewID != "" {
		t.Fatal("refusal changed review authority", state, e)
	}
	t.Log("final refusal", err)
	// Reading the immutable evidence remains possible after revocation.
	for _, id := range evidenceIDs(j) {
		if _, err = f.source.Evidence(ctx, f.owner, id); err != nil {
			t.Fatal("historical evidence unreadable", err)
		}
	}
}

func (f *appFlow) approvedSpec(base catalog.VersionResult, slug string, spec exttest.Spec) (extensions.Package, extensions.PackageRecord) {
	f.t.Helper()
	dir := f.t.TempDir()
	pkg := exttest.Write(f.t, dir, spec)
	v, flow := f.candidate(slug, manifest.TypePlugin, readDir(f.t, dir), map[string]any{"extension_id": pkg.Manifest.ID, "extension_version": pkg.Manifest.Version})
	j := f.checkCandidate(v, flow, "org.lantai.corecheck.manifest")
	target := f.submitChecked(v, flow, base.Ref, j, false)
	f.decide(target.ID, "approve")
	rec, err := f.app.ExtensionManager.Import(f.t.Context(), f.login().Context, f.key(), extensions.ImportRequest{AssetID: v.AssetID, VersionID: v.VersionID})
	if err != nil {
		f.t.Fatal(err)
	}
	return pkg, rec
}
func TestM2GovernanceRealScopesProbeAndRestart(t *testing.T) {
	f := newAppFlow(t)
	ctx := t.Context()
	m := f.app.ExtensionManager
	builtin, err := f.app.Extensions.Producer(ctx, "org.lantai.corecheck.manifest")
	if err != nil {
		t.Fatal(err)
	}
	base := f.approvedProfile("profiles/bootstrap", profileDocument("bootstrap", builtin), ids.PermanentRef{})
	dir := t.TempDir()
	// This case verifies governance and restart, not process startup latency.
	// Use the ordinary fixture budget; dedicated host tests verify short deadlines.
	pkg := exttest.Write(t, dir, exttest.Spec{ID: "org.example.business", Server: true, CLI: true})
	v, flow := f.candidate("plugins/governed", manifest.TypePlugin, readDir(t, dir), map[string]any{"extension_id": pkg.Manifest.ID, "extension_version": pkg.Manifest.Version})
	rec, err := m.Import(ctx, f.login().Context, f.key(), extensions.ImportRequest{AssetID: v.AssetID, VersionID: v.VersionID})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err = f.inst.DB(ownership.Runtime).QueryRowContext(ctx, `SELECT count(*) FROM extensions_activations`).Scan(&n); err != nil || n != 0 {
		t.Fatal("import ran a probe", n, err)
	}
	req := extensions.EnableRequest{ExtensionID: pkg.Manifest.ID, ExtensionVersion: pkg.Manifest.Version, PackageDigest: pkg.Digest, Target: "server", ScopeKind: "instance", Config: json.RawMessage(`{"mode":"pass"}`), ConfigRevision: 1, Trust: extensions.TrustUnenforced, Probe: true, Reason: "explicit synthetic execution"}
	if _, err = m.EnableHumanAction(ctx, req); err == nil {
		t.Fatal("unreviewed package eligible")
	}
	t.Log("unreviewed", errcode.CodeOf(err))
	if _, err = m.Probe(ctx, f.login().Context, ids.New()); errcode.CodeOf(err) != errcode.NotFound {
		t.Fatal("unregistered probe", err)
	}
	j := f.checkCandidate(v, flow, "org.lantai.corecheck.manifest")
	target := f.submitChecked(v, flow, base.Ref, j, false)
	f.decide(target.ID, "approve")
	a, err := m.EnableHumanAction(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	who, g, op := f.grant(a)
	if _, err = m.Enable(ctx, who, req, "", "", f.id); err == nil {
		t.Fatal("execution without grant accepted")
	}
	for _, kind := range []string{"package", "config", "revision", "scope", "target", "operation"} {
		wrong := req
		child := op
		switch kind {
		case "package":
			wrong.PackageDigest = digest.Of([]byte("different"))
		case "config":
			wrong.Config = json.RawMessage(`{"mode":"fail"}`)
		case "revision":
			wrong.ConfigRevision = 2
		case "scope":
			wrong.ScopeKind = "project"
			wrong.ScopeID = f.project.ProjectID
		case "target":
			wrong.Target = "cli"
			wrong.Probe = false
		case "operation":
			child = ids.New()
		}
		if _, err = m.Enable(ctx, who, wrong, g, child, f.id); err == nil {
			t.Fatal("grant crossed binding", kind)
		}
		t.Log("grant refusal", kind, errcode.CodeOf(err))
	}
	noTrust := req
	noTrust.Trust = ""
	if _, err = m.EnableHumanAction(ctx, noTrust); err == nil {
		t.Fatal("unenforced sandbox silently accepted")
	}
	enabled, err := m.Enable(ctx, who, req, g, op, f.id)
	if err != nil || enabled.Activation == nil || enabled.Activation.State != "ready" {
		activation, _ := json.Marshal(enabled.Activation)
		t.Fatalf("enablement probe: %v; activation=%s", err, activation)
	}
	if enabled.Activation.Environment.Capabilities.Sandbox || enabled.Activation.Environment.Capabilities.NetworkIsolation {
		t.Fatal("false sandbox claim")
	}
	if _, _, err = m.Snapshot(ctx, f.project.ProjectID, pkg.Manifest.ID+".check", 1); errcode.CodeOf(err) != errcode.Forbidden {
		t.Fatal("blank allowlist", err)
	}
	p2, err := f.catalog.CreateProject(ctx, catalog.ProjectRequest{Who: f.login().Context, IdempotencyKey: f.key(), Key: "other", Name: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	f.sudo(&identity.SetPolicy{ProjectID: f.project.ProjectID, Key: "plugins.allowed", Value: json.RawMessage(`["org.example.business"]`)})
	if _, _, err = m.Snapshot(ctx, p2.ProjectID, pkg.Manifest.ID+".check", 1); errcode.CodeOf(err) != errcode.Forbidden {
		t.Fatal("cross project", err)
	}
	if _, _, err = m.Snapshot(ctx, f.project.ProjectID, pkg.Manifest.ID+".check", 1); err != nil {
		t.Fatal(err)
	}
	// The CLI is an independent executable speaking real REST to this application.
	servers := remoteServers(t, f.app, true)
	origin := "http://" + servers.Addresses.API
	login := f.login()
	session := filepath.Join(t.TempDir(), "session.json")
	writeJSON(t, session, map[string]string{"schema": "lantai.client-session/v1", "origin": origin, "token": login.Token})
	bin := buildConsistencyCLI(t)
	invoke := func(args ...string) (int, []byte) {
		t.Helper()
		cmd := exec.CommandContext(ctx, bin, append(args, "--server", origin, "--allow-http", "--session-file", session)...)
		out, err := cmd.CombinedOutput()
		code := 0
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
		return code, out
	}
	if code, out := invoke("plugin", "probe", "--id", string(enabled.Enablement.ID)); code != 0 {
		t.Fatal(code, string(out))
	}
	registry := filepath.Join(t.TempDir(), "extensions.json")
	if code, _ := invoke("ext", "install", "--package", dir, "--registry", registry); code == 0 {
		t.Fatal("server authorization became local CLI enablement")
	}
	cliReq := req
	cliReq.Target = "cli"
	cliReq.ScopeKind = "user"
	cliReq.ScopeID = login.Context.PrincipalID
	cliReq.Probe = false
	a, err = m.EnableHumanAction(ctx, cliReq)
	if err != nil {
		t.Fatal(err)
	}
	admin, cg, cop := f.grant(a)
	if _, err = m.Enable(ctx, admin, cliReq, cg, cop, f.id); err != nil {
		t.Fatal(err)
	}
	if code, _ := invoke("ext", "run", pkg.Manifest.ID, "copy", "--registry", registry, "--output", t.TempDir()); code == 0 {
		t.Fatal("untrusted local package ran")
	}
	if code, out := invoke("ext", "install", "--package", dir, "--registry", registry); code != 0 {
		t.Fatal(code, string(out))
	}
	if code, out := invoke("ext", "run", pkg.Manifest.ID, "copy", "--registry", registry, "--arg", "inspect", "--output", t.TempDir()); code != 0 {
		t.Fatal(code, string(out))
	}
	// Replacing the real stored entry bytes invalidates reads/probe; restore them
	// before reopening the instance. No authoritative fact is edited through SQL.
	entry, err := extensions.EntryPath(pkg.Manifest, "server")
	if err != nil {
		t.Fatal(err)
	}
	path := (storage.Layout{Home: f.inst.Layout().Home}).BlobPath(entry.SHA256)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	changed := bytes.Clone(original)
	changed[len(changed)-1] ^= 1
	if err = os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	_, probeErr := m.Probe(ctx, f.login().Context, enabled.Enablement.ID)
	if err = os.WriteFile(path, original, 0400); err != nil {
		t.Fatal(err)
	}
	if probeErr == nil {
		t.Fatal("changed actual entry reused probe")
	}
	t.Log("actual entry refusal", errcode.CodeOf(probeErr))
	if shaOf(original) != entry.SHA256 {
		t.Fatal("original entry hash differs")
	}
	// Independent restart into changed executable bytes must invalidate the old
	// environment probe, even though policy/runtime observations remain stored.
	if err = servers.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	childSession := f.login().Context
	home := f.inst.Layout().Home
	now := f.clk.Now()
	if err = f.app.Close(ctx); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(t.TempDir(), filepath.Base(self))
	exttest.CopyFile(t, self, child)
	file, err := os.OpenFile(child, os.O_WRONLY|os.O_APPEND, 0700)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.Write([]byte("synthetic-core-release-change"))
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	runProbeRestart(t, child, home, now, childSession.SessionID, f.project.ProjectID, pkg.Manifest.ID+".check", "stale")
	// Reopen on the original executable, revoke while old ready observations
	// remain in runtime, then independently restart and verify no resurrection.
	f.app = reopenApplication(t, f.env, storage.Config{MinFreeBytes: 1 << 20})
	f.tasks = f.app.Tasks
	f.flows = f.app.Flows
	f.reviews = f.app.Reviews
	f.source = f.app.Evidence
	f.log = f.app.Events
	m = f.app.ExtensionManager
	disable := extensions.DisableRequest{EnablementID: enabled.Enablement.ID, Mode: "revoke", Reason: "synthetic security revoke"}
	a, err = m.DisableHumanAction(ctx, disable)
	if err != nil {
		t.Fatal(err)
	}
	admin, dg, dop := f.grant(a)
	if _, err = m.Disable(ctx, admin, disable, dg, dop, f.id); err != nil {
		t.Fatal(err)
	}
	// Replaying a completed enable returns its stable receipt and has no effect.
	again, err := m.Enable(ctx, who, req, g, op, f.id)
	if err != nil || again.Receipt.OperationID != enabled.Receipt.OperationID {
		t.Fatal("stable replay differs", again, err)
	}
	if _, err = m.Probe(ctx, f.login().Context, enabled.Enablement.ID); err == nil {
		t.Fatal("revoked probe ran")
	}
	if _, _, err = m.Snapshot(ctx, f.project.ProjectID, pkg.Manifest.ID+".check", 1); err == nil {
		t.Fatal("stale observation resurrected capability")
	}
	childSession = f.login().Context
	now = f.clk.Now()
	if err = f.app.Close(ctx); err != nil {
		t.Fatal(err)
	}
	runProbeRestart(t, self, home, now, childSession.SessionID, f.project.ProjectID, pkg.Manifest.ID+".check", "disabled")
	t.Logf("package=%s entry=%s review_ref=%s isolation=%s", pkg.Digest, entry.SHA256, rawJSON(rec.Ref), rawJSON(enabled.Activation.Environment.Capabilities))
}
func runProbeRestart(t *testing.T, bin, home string, now time.Time, session, project ids.ID, processor, want string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), bin, "-test.run=^TestM2ProbeRestartHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "LANTAI_PROBE_TEST_HOME="+home, "LANTAI_PROBE_TEST_NOW="+clock.Format(now), "LANTAI_PROBE_TEST_SESSION="+string(session), "LANTAI_PROBE_TEST_PROJECT="+string(project), "LANTAI_PROBE_TEST_PROCESSOR="+processor, "LANTAI_PROBE_TEST_WANT="+want)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("independent restart: %v\n%s", err, out)
	}
	t.Log(string(out))
}
func TestM2ProbeRestartHelper(t *testing.T) {
	home := os.Getenv("LANTAI_PROBE_TEST_HOME")
	if home == "" {
		t.Skip("independent restart helper")
	}
	now, err := clock.Parse(os.Getenv("LANTAI_PROBE_TEST_NOW"))
	if err != nil {
		t.Fatal(err)
	}
	app, err := application.Open(t.Context(), application.Options{Instance: operations.Options{Home: home, Clock: clock.NewFake(now)}, Identity: identity.Config{Password: fastPassword}, Storage: storage.Config{MinFreeBytes: 1 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(context.Background())
	who, err := app.Identity.VerifySession(t.Context(), ids.ID(os.Getenv("LANTAI_PROBE_TEST_SESSION")))
	if err != nil {
		t.Fatal(err)
	}
	views, err := app.ExtensionManager.Enablements(t.Context(), who)
	if err != nil {
		t.Fatal(err)
	}
	wanted := os.Getenv("LANTAI_PROBE_TEST_WANT")
	reason := "probe_stale"
	if wanted == "disabled" {
		reason = "disabled"
	}
	found := false
	for _, v := range views {
		if v.Target == "server" {
			found = slices.Contains(v.Reasons, reason)
			if wanted == "stale" {
				if v.Activation == nil || v.Activation.State != "ready" {
					t.Fatal("old probe observation not retained", v)
				}
			} else {
				var count int
				if err = app.Instance.DB(ownership.Runtime).QueryRowContext(t.Context(), `SELECT count(*) FROM extensions_activations WHERE enablement_id=? AND generation<? AND state='ready'`, v.ID, v.Generation).Scan(&count); err != nil || count < 1 {
					t.Fatal("old ready generation not retained", count, err)
				}
			}
			t.Log("restart current refusal", v.Reasons)
		}
	}
	if !found {
		t.Fatal("restart did not invalidate old authority", views)
	}
	if _, _, err = app.ExtensionManager.Snapshot(t.Context(), ids.ID(os.Getenv("LANTAI_PROBE_TEST_PROJECT")), os.Getenv("LANTAI_PROBE_TEST_PROCESSOR"), 1); err == nil {
		t.Fatal("restart dispatched stale probe/authority")
	}
}

func TestM2GovernanceProbeFailuresAndLateRevoke(t *testing.T) {
	f := newAppFlow(t)
	ctx := t.Context()
	m := f.app.ExtensionManager
	builtin, err := f.app.Extensions.Producer(ctx, "org.lantai.corecheck.manifest")
	if err != nil {
		t.Fatal(err)
	}
	base := f.approvedProfile("profiles/bootstrap", profileDocument("bootstrap", builtin), ids.PermanentRef{})
	for index, mode := range []string{"probe_crash", "probe_hang", "probe_big_output", "probe_bad_schema", "probe_no_result", "probe_unsupported", "probe_records"} {
		pkg, _ := f.approvedSpec(base, "plugins/"+mode, exttest.Spec{ID: "org.example." + strings.ReplaceAll(mode, "_", ""), Server: true, TimeoutSeconds: 1})
		req := extensions.EnableRequest{ExtensionID: pkg.Manifest.ID, ExtensionVersion: pkg.Manifest.Version, PackageDigest: pkg.Digest, Target: "server", ScopeKind: "instance", Config: json.RawMessage(`{"mode":"` + mode + `"}`), ConfigRevision: 1, Trust: extensions.TrustUnenforced, Probe: true, Reason: "synthetic probe fault"}
		a, err := m.EnableHumanAction(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		who, g, op := f.grant(a)
		result, err := m.Enable(ctx, who, req, g, op, f.id)
		if err == nil || result.Activation == nil || result.Activation.State == "ready" {
			t.Errorf("invalid probe became ready: mode=%s error=%v activation=%+v", mode, err, result.Activation)
		} else {
			t.Log("probe refusal", mode, result.Activation.Failure)
		}
		f.sudo(&identity.SetPolicy{ProjectID: f.project.ProjectID, Key: "plugins.allowed", Value: json.RawMessage(`["` + pkg.Manifest.ID + `"]`), ExpectedRevision: int64(index)})
		if _, _, err = m.Snapshot(ctx, f.project.ProjectID, pkg.Manifest.ID+".check", 1); err == nil {
			t.Errorf("failed probe dispatched: %s", mode)
		}
	}
	pkg, _ := f.approvedSpec(base, "plugins/delayedprobe", exttest.Spec{ID: "org.example.delayprobe", Server: true, TimeoutSeconds: 3})
	enabled := f.enablePackage(pkg, `{"mode":"probe_delay","sleep_ms":1500}`, 1)
	who := f.login().Context
	done := make(chan error, 1)
	go func() { _, e := m.Probe(ctx, who, enabled.Enablement.ID); done <- e }()
	limit := time.Now().Add(5 * time.Second)
	for {
		var state string
		err = f.inst.DB(ownership.Runtime).QueryRowContext(ctx, `SELECT state FROM extensions_activations WHERE enablement_id=? AND generation=?`, enabled.Enablement.ID, enabled.Enablement.Generation).Scan(&state)
		if err != nil {
			t.Fatal(err)
		}
		if state == "pending_probe" {
			break
		}
		if time.Now().After(limit) {
			t.Fatal("probe did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	request := extensions.DisableRequest{EnablementID: enabled.Enablement.ID, Mode: "revoke", Reason: "revoke during actual pending probe"}
	a, err := m.DisableHumanAction(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	admin, g, op := f.grant(a)
	if _, err = m.Disable(ctx, admin, request, g, op, f.id); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err == nil {
		t.Error("late probe success accepted after security revoke")
	} else {
		t.Log("late probe refusal", errcode.CodeOf(err))
	}
}
func TestM2GovernanceUnsupportedAndImmutableImport(t *testing.T) {
	f := newAppFlow(t)
	ctx := t.Context()
	m := f.app.ExtensionManager
	for _, kind := range []string{"node", "web", "tool", "network", "builtin"} {
		spec := exttest.Spec{ID: "org.example.unsupported" + kind, Server: true, Mutate: func(doc map[string]any) {
			switch kind {
			case "node", "web":
				target := doc["targets"].(map[string]any)
				target[kind] = target["server"]
				delete(target, "server")
				doc["contributes"].([]map[string]any)[0]["target"] = kind
			case "tool":
				doc["targets"].(map[string]any)["server"].(map[string]any)["processor"].(map[string]any)["requires"] = []string{"synthetic-tool"}
			case "network":
				doc["permissions"].(map[string]any)["network"] = []string{"https://example.invalid"}
			case "builtin":
				doc["id"] = "org.lantai.corecheck"
				doc["contributes"].([]map[string]any)[0]["id"] = "org.lantai.corecheck.manifest"
			}
		}}
		dir := t.TempDir()
		pkg := exttest.Write(t, dir, spec)
		v, _ := f.candidate("plugins/unsupported-"+kind, manifest.TypePlugin, readDir(t, dir), map[string]any{"extension_id": pkg.Manifest.ID, "extension_version": pkg.Manifest.Version})
		if _, err := m.Import(ctx, f.login().Context, f.key(), extensions.ImportRequest{AssetID: v.AssetID, VersionID: v.VersionID}); err == nil {
			t.Fatal("unsupported imported", kind)
		} else {
			t.Log("unsupported refusal", kind, errcode.CodeOf(err))
		}
	}
	dir := t.TempDir()
	pkg := exttest.Write(t, dir, exttest.Spec{ID: "org.example.immutable", Server: true})
	v, _ := f.candidate("plugins/immutable", manifest.TypePlugin, readDir(t, dir), map[string]any{"extension_id": pkg.Manifest.ID, "extension_version": pkg.Manifest.Version})
	rec, err := m.Import(ctx, f.login().Context, f.key(), extensions.ImportRequest{AssetID: v.AssetID, VersionID: v.VersionID})
	if err != nil {
		t.Fatal(err)
	}
	dir2 := t.TempDir()
	other := exttest.Write(t, dir2, exttest.Spec{ID: pkg.Manifest.ID, Version: pkg.Manifest.Version, Server: true, TimeoutSeconds: 2})
	replacement, _ := f.candidate("plugins/replacement", manifest.TypePlugin, readDir(t, dir2), map[string]any{"extension_id": other.Manifest.ID, "extension_version": other.Manifest.Version})
	if _, err = m.Import(ctx, f.login().Context, f.key(), extensions.ImportRequest{AssetID: replacement.AssetID, VersionID: replacement.VersionID}); errcode.CodeOf(err) != errcode.IdempotencyConflict {
		t.Fatal("same version replaced", err)
	}
	list, err := m.Packages(ctx, f.login().Context)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].PackageDigest != rec.PackageDigest {
		t.Fatal("refusal changed registration", list)
	}
}

func TestM2GovernanceRealJobFaultResults(t *testing.T) {
	f := newAppFlow(t)
	ctx := t.Context()
	builtin, err := f.app.Extensions.Producer(ctx, "org.lantai.corecheck.manifest")
	if err != nil {
		t.Fatal(err)
	}
	base := f.approvedProfile("profiles/bootstrap", profileDocument("bootstrap", builtin), ids.PermanentRef{})
	// Probe startup must succeed before exercising each job fault. The hanging
	// job still reaches its dispatch deadline and must be reclaimed as a fault.
	pkg, _ := f.approvedSpec(base, "plugins/faultjob", exttest.Spec{ID: "org.example.faultjob", Server: true})
	f.sudo(&identity.SetPolicy{ProjectID: f.project.ProjectID, Key: "plugins.allowed", Value: json.RawMessage(`["org.example.faultjob"]`)})
	for index, mode := range []string{"crash", "hang", "bad_schema", "no_result", "unsupported"} {
		f.enablePackage(pkg, `{"mode":"`+mode+`"}`, int64(index+1))
		v, flow := f.candidate("docs/"+mode, manifest.TypeDoc, map[string][]byte{"data.json": []byte(`{"valid":true}`)}, nil)
		j := f.checkCandidate(v, flow, pkg.Manifest.ID+".check")
		if j.State != "failed" || len(j.Checks) != 0 || len(j.Evidence) != 0 || j.Attempt.Verdict != execution.VerdictUnknown {
			t.Fatal("fault became evidence", mode, j)
		}
		want := "runtime_fault"
		if mode == "unsupported" {
			want = "unsupported_input"
		}
		if j.Failure != want {
			t.Fatal(mode, j.Failure)
		}
		if mode != "unsupported" && (j.Attempt.Outcome != execution.InvocationRuntimeFault || !j.Attempt.Termination.Confirmed || j.Attempt.Termination.UnresolvedEffects != 0) {
			t.Fatal("runtime fault was not reclaimed", mode, j.Attempt)
		}
		if mode == "unsupported" && (!j.Attempt.ResultDigest.Valid() || j.Attempt.Outcome != execution.InvocationCompleted) {
			t.Fatal("unsupported lost its completed result digest", j)
		}
		t.Log("job fault refusal", mode, j.Failure, j.Attempt.Termination.Confirmed)
	}
}

func TestM2GovernanceRealJobDrainAndRevoke(t *testing.T) {
	for _, mode := range []string{"drain", "revoke"} {
		t.Run(mode, func(t *testing.T) {
			f := newAppFlow(t)
			ctx := t.Context()
			m := f.app.ExtensionManager
			builtin, err := f.app.Extensions.Producer(ctx, "org.lantai.corecheck.manifest")
			if err != nil {
				t.Fatal(err)
			}
			base := f.approvedProfile("profiles/bootstrap", profileDocument("bootstrap", builtin), ids.PermanentRef{})
			pkg, _ := f.approvedPackage(base, "plugins/business", "0.1.0")
			enabled := f.enablePackage(pkg, `{"mode":"business","sleep_ms":1500}`, 1)
			processor := pkg.Manifest.ID + ".check"
			f.sudo(&identity.SetPolicy{ProjectID: f.project.ProjectID, Key: "plugins.allowed", Value: json.RawMessage(`["org.example.business"]`)})
			v, flow := f.candidate("docs/inflight", manifest.TypeDoc, map[string][]byte{"data.json": []byte(`{"valid":true}`)}, map[string]any{"synthetic_valid": true})
			// Prepare the grant before dispatch so TOTP's fake-clock advance cannot
			// expire the independent 30-second Job lease during this timing test.
			request := extensions.DisableRequest{EnablementID: enabled.Enablement.ID, Mode: mode, Reason: "synthetic in-flight retirement"}
			a, err := m.DisableHumanAction(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			who, g, op := f.grant(a)
			ref, err := f.app.Jobs.StartJob(ctx, f.owner, workflow.JobRequest{OperationID: ids.New(), ProjectID: f.project.ProjectID, Processor: processor, Target: v.Ref, Flow: flow, ManifestDigest: string(v.ManifestDigest)})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.app.Nodes.Observe(ctx, f.worker, f.key(), node.Observation{ProjectID: f.project.ProjectID, Capabilities: []string{processor}, Slots: 1}); err != nil {
				t.Fatal(err)
			}
			j, err := f.app.Jobs.Read(ctx, f.owner, ref.JobID)
			if err != nil {
				t.Fatal(err)
			}
			type result struct {
				job jobs.Job
				err error
			}
			done := make(chan result, 1)
			key := f.key()
			go func() {
				out, e := f.app.Jobs.Run(ctx, f.worker, key, jobs.Control{JobID: j.ID, ExpectedRevision: j.Revision})
				done <- result{out, e}
			}()
			limit := time.Now().Add(5 * time.Second)
			for {
				var count int
				if err = f.inst.DB(ownership.Runtime).QueryRowContext(ctx, `SELECT count(*) FROM extensions_invocations WHERE enablement_id=? AND generation=? AND state='dispatched'`, enabled.Enablement.ID, enabled.Enablement.Generation).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count > 0 {
					break
				}
				if time.Now().After(limit) {
					t.Fatal("actual job not admitted")
				}
				time.Sleep(10 * time.Millisecond)
			}
			time.Sleep(50 * time.Millisecond)
			if _, err = m.Disable(ctx, who, request, g, op, f.id); err != nil {
				t.Fatal(err)
			}
			out := <-done
			if out.err != nil {
				t.Fatal(out.err)
			}
			if mode == "drain" {
				if out.job.State != "succeeded" || len(out.job.Evidence) != 5 {
					t.Fatal("normal drain lost admitted result", out.job)
				}
			} else {
				if out.job.State == "succeeded" || len(out.job.Evidence) != 0 || !out.job.Attempt.Termination.Confirmed {
					t.Fatal("revoked process result accepted", out.job)
				}
			}
			if _, _, err = m.Snapshot(ctx, f.project.ProjectID, processor, 1); err == nil {
				t.Fatal("retired activation admitted a new call")
			}
			t.Log("actual in-flight retirement", mode, out.job.State, out.job.Failure, "stop_confirmed", out.job.Attempt.Termination.Confirmed, "evidence", len(out.job.Evidence))
		})
	}
}

func TestM2BusinessCoreIntegrityCannotBeWaived(t *testing.T) {
	f := newAppFlow(t)
	ctx := t.Context()
	builtin, err := f.app.Extensions.Producer(ctx, "org.lantai.corecheck.manifest")
	if err != nil {
		t.Fatal(err)
	}
	base := f.approvedProfile("profiles/bootstrap", profileDocument("bootstrap", builtin), ids.PermanentRef{})
	pkg, _ := f.approvedPackage(base, "plugins/business", "0.1.0")
	enabled := f.enablePackage(pkg, `{"mode":"business"}`, 1)
	f.sudo(&identity.SetPolicy{ProjectID: f.project.ProjectID, Key: "plugins.allowed", Value: json.RawMessage(`["org.example.business"]`)})
	producer := storage.Producer{ExtensionID: pkg.Manifest.ID, ExtensionVersion: pkg.Manifest.Version, PackageDigest: pkg.Digest, Source: "package", ContributionID: pkg.Manifest.ID + ".check"}
	profile := f.approvedProfile("profiles/media", businessProfile("media", producer, enabled.Enablement.ConfigDigest), base.Ref)
	original := []byte(`{"synthetic":"integrity failure only"}`)
	v, flow := f.candidate("docs/core-integrity", manifest.TypeDoc, map[string][]byte{"data.json": original}, map[string]any{"synthetic_valid": false})
	path := f.app.Storage.Layout().BlobPath(digest.Of(original).Hex())
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte(`{"synthetic":"corrupt fixture bytes"}`), 0600); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if e := os.Chmod(path, 0600); e != nil {
			t.Error(e)
		}
		if e := os.WriteFile(path, original, 0400); e != nil {
			t.Error(e)
		}
		if e := os.Chmod(path, 0400); e != nil {
			t.Error(e)
		}
	}()
	j := f.checkCandidate(v, flow, producer.ContributionID)
	if len(j.Checks) != 5 || j.Checks[0].Check.Verdict != execution.VerdictFail || j.Checks[4].Check.Verdict != execution.VerdictFail {
		t.Fatal("synthetic corruption not observed", j)
	}
	// Restore immutable bytes after observing the real core integrity failure.
	if err = os.WriteFile(path, original, 0400); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	target := f.submitChecked(v, flow, profile.Ref, j, false)
	waiver := ledger.ReviewWaiver{CheckKey: "media_structure", Reason: "ordinary synthetic business exception", EvidenceIDs: []ids.ID{j.Evidence[4].EvidenceID}, ExpiresAt: clock.Format(f.clk.Now().Add(24 * time.Hour))}
	if _, err = f.recordDecision(f.reviewDecision(target, []ledger.ReviewWaiver{waiver})); errcode.CodeOf(err) != errcode.ChecksNotSatisfied {
		t.Fatal("core integrity failure waived", err)
	}
	t.Log("real core integrity failure cannot be bypassed by an ordinary business waiver")
}

// Capture only synthetic immutable evidence; sessions and secrets are never read.
func captureGovernanceEvidence(t *testing.T, name string, raw []byte) {
	t.Helper()
	root := os.Getenv("LANTAI_TEST_EVIDENCE_DIR")
	if root == "" {
		return
	}
	path := filepath.Join(root, strings.NewReplacer("/", "_", "\\", "_").Replace(t.Name()), name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0400); err != nil {
		t.Fatal(err)
	}
	t.Log("synthetic evidence artifact", name, digest.Of(raw))
}

func TestM2BusinessRepeatedIdenticalResultKeepsExactRun(t *testing.T) {
	f := newAppFlow(t)
	ctx := t.Context()
	m := f.app.ExtensionManager
	builtin, err := f.app.Extensions.Producer(ctx, "org.lantai.corecheck.manifest")
	if err != nil {
		t.Fatal(err)
	}
	base := f.approvedProfile("profiles/bootstrap", profileDocument("bootstrap", builtin), ids.PermanentRef{})
	pkg, _ := f.approvedPackage(base, "plugins/business", "0.1.0")
	enabled := f.enablePackage(pkg, `{"mode":"business"}`, 1)
	f.sudo(&identity.SetPolicy{ProjectID: f.project.ProjectID, Key: "plugins.allowed", Value: json.RawMessage(`["org.example.business"]`)})
	producer := storage.Producer{ExtensionID: pkg.Manifest.ID, ExtensionVersion: pkg.Manifest.Version, PackageDigest: pkg.Digest, Source: "package", ContributionID: pkg.Manifest.ID + ".check"}
	profile := f.approvedProfile("profiles/media", businessProfile("media", producer, enabled.Enablement.ConfigDigest), base.Ref)
	v, flow := f.candidate("docs/identical", manifest.TypeDoc, map[string][]byte{"data.json": []byte(`{"valid":true}`)}, map[string]any{"synthetic_valid": true})
	old := f.checkCandidate(v, flow, producer.ContributionID)
	request := extensions.DisableRequest{EnablementID: enabled.Enablement.ID, Mode: "revoke", Reason: "retire original identical result execution"}
	a, err := m.DisableHumanAction(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	who, g, op := f.grant(a)
	if _, err = m.Disable(ctx, who, request, g, op, f.id); err != nil {
		t.Fatal(err)
	}
	f.enablePackage(pkg, `{"mode":"business"}`, 1)
	fresh := f.checkCandidate(v, flow, producer.ContributionID)
	if fresh.State != "succeeded" || len(fresh.Evidence) != 5 || fresh.Checks[4].CheckRunID != fresh.Attempt.ID || old.Checks[4].CheckRunID != old.Attempt.ID {
		t.Fatal("new identical run could not accept its own result", fresh)
	}
	committed, err := f.ledger.Version(ctx, v.AssetID, v.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := f.source.Evidence(ctx, f.owner, old.Evidence[4].EvidenceID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.source.CheckApplicable(ctx, f.owner, committed, stale); errcode.CodeOf(err) != errcode.ExtensionActivationStale {
		t.Fatal("old record borrowed new execution authority", err)
	}
	target := f.submitChecked(v, flow, profile.Ref, fresh, false)
	if _, err = f.recordDecision(f.reviewDecision(target, []ledger.ReviewWaiver{})); err != nil {
		t.Fatal("fresh run was confused with revoked identical result", err)
	}
	t.Log("exact independent job runs", old.Attempt.ID, fresh.Attempt.ID, "same candidate/package/config preserved")
}
