package ledger

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/extensions"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
)

// These fixtures explicitly simulate the T05/T06 read ports. Production human
// proof is exercised separately with identity's TOTP implementation.
type reviewSourcesFixture struct {
	documents map[ids.PermanentRef]catalog.ProjectDocument
	profile   ProfileSnapshot
	evidence  map[ids.ID]AcceptedEvidence
	taskErr   error
	useErr    error
	// taskHook, when set, replaces taskErr and sees the caller's context.
	taskHook func(context.Context) error
}

func (s *reviewSourcesFixture) Profile(context.Context, authz.Context, ids.PermanentRef) (ProfileSnapshot, error) {
	return s.profile, nil
}
func (s *reviewSourcesFixture) Evidence(_ context.Context, _ authz.Context, id ids.ID) (AcceptedEvidence, error) {
	e, ok := s.evidence[id]
	if !ok {
		return e, errcode.New(errcode.NotFound, "")
	}
	return e, nil
}
func (s *reviewSourcesFixture) Task(ctx context.Context, _ authz.Context, _ commit.Committed, _ ReviewFlow, _ string) error {
	if s.taskHook != nil {
		return s.taskHook(ctx)
	}
	return s.taskErr
}
func (s *reviewSourcesFixture) Use(context.Context, authz.Context, commit.Committed, authz.Purpose) error {
	return s.useErr
}
func (s *reviewSourcesFixture) AssetType(_ context.Context, v commit.Committed) (manifest.AssetType, error) {
	if v.VersionID == s.profile.Ref.VersionID {
		return manifest.TypeConfig, nil
	}
	return manifest.TypeDoc, nil
}

type reviewPoliciesFixture map[string]identity.PolicyValue

func (p reviewPoliciesFixture) ResolvePolicies(context.Context, ids.ID) (map[string]identity.PolicyValue, error) {
	return p, nil
}

type reviewFixture struct {
	*fixture
	r     *Reviews
	src   *reviewSourcesFixture
	pol   reviewPoliciesFixture
	v     commit.Committed
	flow  ReviewFlow
	input SubmitReview
}

func newReviewFixture(t *testing.T) *reviewFixture {
	t.Helper()
	f := newFixture(t)
	f.az.Grant(f.who.PrincipalID, f.project, "ledger.submit_review", "ledger.publish")
	pp, proof := f.prepare(t, "approved-profile-fixture")
	profile, err := f.s.Commit(t.Context(), pp.OperationID, f.who, proof)
	if err != nil {
		t.Fatal(err)
	}
	// This is only an existing approved-profile seed, not an approval API.
	if _, err = f.db.ExecContext(t.Context(), `UPDATE ledger_version_states SET state='approved',effective_review_id=? WHERE version_id=?`, ids.New(), profile.VersionID); err != nil {
		t.Fatal(err)
	}
	p, proof := f.prepare(t, "reviewed")
	v, err := f.s.Commit(t.Context(), p.OperationID, f.who, proof)
	if err != nil {
		t.Fatal(err)
	}
	producer := storage.Producer{ExtensionID: "org.example.check", ExtensionVersion: "1.0.0", PackageDigest: digest.Of([]byte("check-package")), Source: "package", ContributionID: "org.example.check.all"}
	config := digest.Of([]byte("fixed-config"))
	ap := AcceptanceProfile{Contract: "lantai.acceptance-profile/v1", ID: "basic", Revision: 1, AssetTypes: []manifest.AssetType{manifest.TypeDoc}, Purpose: authz.PurposeProduction, RequiredEvidence: []string{}, DistinctActorRule: "maker_checker", WaivableChecks: []string{}, Defaults: ProfileDefaults{Publication: "auto"}}
	src := &reviewSourcesFixture{profile: ProfileSnapshot{Ref: profile.Ref(f.who.InstanceID), ManifestDigest: profile.ManifestDigest}, evidence: map[ids.ID]AcceptedEvidence{}}
	flow := ReviewFlow{TaskID: ids.New(), AttemptID: ids.New(), Round: 1, Fence: 1}
	list := []ids.ID{}
	for _, key := range []string{"integrity", "schema", "license_evidence", "purpose"} {
		ap.RequiredChecks = append(ap.RequiredChecks, CheckRequirement{Key: key, SchemaVersion: 1, AcceptedProcessors: []storage.Producer{producer}, ConfigDigest: config, Severity: "error"})
		id := ids.New()
		list = append(list, id)
		src.evidence[id] = AcceptedEvidence{ID: id, Digest: digest.Of([]byte(key)), Ref: v.Ref(f.who.InstanceID), ManifestDigest: v.ManifestDigest, Kind: "check_result", ActorID: ids.New(), Flow: flow, CheckKey: key, SchemaVersion: 1, ConfigDigest: config, Check: &extensions.CheckResult{Contract: "lantai.check-result/v1", Ref: v.Ref(f.who.InstanceID), ManifestDigest: v.ManifestDigest, Verdict: "pass", Findings: []string{}, Producer: producer}, CompletedAt: clock.Format(f.clk.Now())}
	}
	src.profile.Profile = ap
	pol := reviewPoliciesFixture{"review.allow_self_human": {Value: json.RawMessage(`true`)}}
	r, err := f.s.NewReviews(src, pol)
	if err != nil {
		t.Fatal(err)
	}
	return &reviewFixture{f, r, src, pol, v, flow, SubmitReview{ProjectID: f.project, VersionID: v.VersionID, ExpectedRevision: 1, ProfileRef: src.profile.Ref, EvidenceIDs: list, Flow: flow}}
}
func (f *reviewFixture) submit(t *testing.T) ReviewTarget {
	t.Helper()
	out, err := f.r.Submit(t.Context(), f.who, "submit", f.input)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func (f *reviewFixture) decide(t *testing.T, target ReviewTarget, in ReviewDecision) (Review, error) {
	t.Helper()
	in.TargetID = target.ID
	if in.Action == "" {
		in.Action = identity.ActRecordReview
	}
	if in.ExpectedRevision == 0 {
		in.ExpectedRevision = target.Revision
	}
	if in.Reason == "" {
		in.Reason = "synthetic review decision"
	}
	ctx, h, err := f.gate.Acquire(t.Context(), commands.Request{Security: commands.ModeExclusive, Projects: []string{string(f.project)}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	cmd := f.command(string(in.Action), f.project, string(ids.New()), in)
	cmd.HumanGrantID = ids.New()
	receipt, err := (&reviewCommand{r: f.r, who: f.who, in: in}).Commit(ctx, cmd, identity.HumanAction{})
	if err != nil {
		return Review{}, err
	}
	var out Review
	err = json.Unmarshal(receipt.ResponseSummary, &out)
	return out, err
}
func TestReviewApprovePublishRevokeAndReplay(t *testing.T) {
	f := newReviewFixture(t)
	target := f.submit(t)
	wantAliases(t, f.s, f.v.AssetID, "", "")
	replay, err := f.r.Submit(t.Context(), f.who, "submit", f.input)
	if err != nil || replay.ID != target.ID {
		t.Fatal(replay, err)
	}
	review, err := f.decide(t, target, ReviewDecision{Verdict: "approve"})
	if err != nil || !review.PublicationPending || !review.SelfReview {
		t.Fatal(review, err)
	}
	a, err := f.s.AssetControl(t.Context(), f.v.AssetID)
	if err != nil || a.PublicationState != "unpublished" {
		t.Fatal("approval must not publish", a, err)
	}
	wantAliases(t, f.s, f.v.AssetID, "", f.v.VersionID)
	requests, err := f.r.PublicationRequests(t.Context(), f.who, f.v.AssetID)
	if err != nil || len(requests) != 1 {
		t.Fatal(requests, err)
	}
	pub, err := f.r.RunPublication(t.Context(), f.who, requests[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.r.RunPublication(t.Context(), f.who, requests[0].ID)
	if err != nil || pub.ID != again.ID {
		t.Fatal(again, err)
	}
	wantAliases(t, f.s, f.v.AssetID, f.v.VersionID, f.v.VersionID)
	// After the flow completes the dispatcher no longer holds execution rights;
	// replaying the succeeded request still returns the first receipt
	// (BUG-20261001-06) and adds no history.
	f.src.taskErr = errcode.New(errcode.InvalidStateTransition, "").WithDetails(errcode.Detail{Reason: "flow_completed"})
	completed, err := f.r.RunPublication(t.Context(), f.who, requests[0].ID)
	if err != nil || completed.ID != pub.ID || completed.OperationID != pub.OperationID || completed.Revision != pub.Revision {
		t.Fatal("replay after flow completion", completed, err)
	}
	// Another caller has no receipt for this request; without one it must be the
	// current flow executor, and the completed flow refuses it.
	otherID := ids.New()
	f.az.AddPrincipal(otherID, authz.Human)
	f.az.Grant(otherID, f.project, "catalog.read")
	other := f.az.OpenSession(otherID, time.Hour)
	_, err = f.r.RunPublication(t.Context(), other, requests[0].ID)
	wantCode(t, err, errcode.InvalidStateTransition)
	if n := publicationCount(t, f); n != 1 {
		t.Fatal("replays added publication history", n)
	}
	if requests, err = f.r.PublicationRequests(t.Context(), f.who, f.v.AssetID); err != nil || requests[0].Status != "succeeded" {
		t.Fatal("a refused replay changed the request status", requests, err)
	}
	_, err = f.decide(t, target, ReviewDecision{Action: identity.ActRevokeReview, ExpectedRevision: 3, EffectiveReviewID: review.ID, Verdict: "revoke"})
	if err != nil {
		t.Fatal(err)
	}
	// A completed old operation may return its historical receipt, but cannot
	// update the publication pointer after the same-transaction suspension.
	if old, err := f.r.RunPublication(t.Context(), f.who, requests[0].ID); err != nil || old.ID != pub.ID {
		t.Fatal(old, err)
	}
	if n := publicationCount(t, f); n != 2 {
		t.Fatal("history must hold the release and its suspension only", n)
	}
	a, err = f.s.AssetControl(t.Context(), f.v.AssetID)
	if err != nil || a.PublicationState != "suspended" || a.PublishedVersionID != "" || a.PublicationRevision != 2 {
		t.Fatal(a, err)
	}
	state, _ := f.s.VersionControl(t.Context(), f.v.VersionID)
	if state.ReviewState != "withdrawn" || state.EffectiveReviewID != "" {
		t.Fatal(state)
	}
	wantAliases(t, f.s, f.v.AssetID, "", "")
	// A new production round brings back an active flow for the resubmission.
	f.src.taskErr = nil
	f.input.ExpectedRevision = state.Revision
	next, err := f.r.Submit(t.Context(), f.who, "resubmit", f.input)
	if err != nil || next.ID == target.ID {
		t.Fatal(next, err)
	}
	_, err = f.decide(t, target, ReviewDecision{Verdict: "approve"})
	wantCode(t, err, errcode.ReviewTargetStale)
}
func TestReviewRejectsChangedEvidenceFenceAndSelfPolicy(t *testing.T) {
	for _, test := range []string{"manifest", "round", "processor_version", "package", "config", "fence", "unknown", "use", "self", "qa_self", "expired_waiver", "mandatory_waiver"} {
		t.Run(test, func(t *testing.T) {
			f := newReviewFixture(t)
			if test == "qa_self" {
				e := AcceptedEvidence{ID: ids.New(), Digest: digest.Of([]byte("qa")), Ref: f.v.Ref(f.who.InstanceID), ManifestDigest: f.v.ManifestDigest, Kind: "qa_report", QAVerdict: "pass", ActorID: f.v.CommittedBy, Flow: f.flow, CompletedAt: clock.Format(f.clk.Now())}
				f.src.evidence[e.ID] = e
				f.input.EvidenceIDs = append(f.input.EvidenceIDs, e.ID)
				f.src.profile.Profile.QARequired = true
			}
			if test == "expired_waiver" {
				f.src.profile.Profile.RequiredChecks = append(f.src.profile.Profile.RequiredChecks, CheckRequirement{Key: "visual", SchemaVersion: 1, AcceptedProcessors: f.src.profile.Profile.RequiredChecks[0].AcceptedProcessors, ConfigDigest: f.src.profile.Profile.RequiredChecks[0].ConfigDigest, Severity: "warning", Waivable: true})
				f.src.profile.Profile.WaivableChecks = []string{"visual"}
				e := f.src.evidence[f.input.EvidenceIDs[0]]
				e.ID = ids.New()
				e.CheckKey = "visual"
				copy := *e.Check
				copy.Verdict = "fail"
				e.Check = &copy
				f.src.evidence[e.ID] = e
				f.input.EvidenceIDs = append(f.input.EvidenceIDs, e.ID)
			}
			target := f.submit(t)
			decision := ReviewDecision{Verdict: "approve"}
			expected := errcode.ReviewTargetStale
			e := f.src.evidence[f.input.EvidenceIDs[0]]
			switch test {
			case "manifest":
				e.ManifestDigest = digest.Of([]byte("wrong manifest"))
				expected = errcode.ChecksNotSatisfied
			case "round":
				e.Flow.Round++
				expected = errcode.ChecksNotSatisfied
			case "processor_version":
				e.Check.Producer.ExtensionVersion = "2.0.0"
			case "package":
				e.Check.Producer.PackageDigest = digest.Of([]byte("replacement"))
			case "config":
				e.ConfigDigest = digest.Of([]byte("changed"))
			case "fence":
				f.src.taskErr = errcode.New(errcode.LeaseStale, "")
				expected = errcode.LeaseStale
			case "unknown":
				e.Check.Verdict = "unknown"
			case "use":
				f.src.useErr = errcode.New(errcode.UseRestricted, "")
				expected = errcode.UseRestricted
			case "self":
				f.pol["review.allow_self_human"] = identity.PolicyValue{Value: json.RawMessage(`false`)}
				expected = errcode.SelfReviewForbidden
			case "qa_self":
				expected = errcode.SelfReviewForbidden
			case "expired_waiver":
				decision.Waivers = []ReviewWaiver{{CheckKey: "visual", Reason: "test", EvidenceIDs: []ids.ID{e.ID}, ExpiresAt: clock.Format(f.clk.Now().Add(-time.Minute))}}
				expected = errcode.ChecksNotSatisfied
			case "mandatory_waiver":
				decision.Waivers = []ReviewWaiver{{CheckKey: "integrity", Reason: "test", EvidenceIDs: []ids.ID{e.ID}}}
				expected = errcode.ChecksNotSatisfied
			}
			f.src.evidence[e.ID] = e
			_, err := f.decide(t, target, decision)
			wantCode(t, err, expected)
			state, _ := f.s.VersionControl(t.Context(), f.v.VersionID)
			if state.ReviewState != "submitted" {
				t.Fatal(state)
			}
			var count int
			if err = f.db.QueryRowContext(t.Context(), `SELECT count(*) FROM ledger_reviews`).Scan(&count); err != nil || count != 0 {
				t.Fatal(count, err)
			}
		})
	}
}
func TestReviewFailedPublicationIsPersistentAndCannotRevive(t *testing.T) {
	f := newReviewFixture(t)
	target := f.submit(t)
	review, err := f.decide(t, target, ReviewDecision{Verdict: "approve"})
	if err != nil {
		t.Fatal(err)
	}
	q, err := f.r.PublicationRequests(t.Context(), f.who, f.v.AssetID)
	if err != nil {
		t.Fatal(err)
	}
	f.src.useErr = errcode.New(errcode.UseRestricted, "")
	_, err = f.r.RunPublication(t.Context(), f.who, q[0].ID)
	wantCode(t, err, errcode.UseRestricted)
	q, err = f.r.PublicationRequests(t.Context(), f.who, f.v.AssetID)
	if err != nil || q[0].Status != "failed" || q[0].FailureCode != "USE_RESTRICTED" {
		t.Fatal(q, err)
	}
	f.src.useErr = nil
	_, err = f.decide(t, target, ReviewDecision{Action: identity.ActRevokeReview, Verdict: "revoke", EffectiveReviewID: review.ID, ExpectedRevision: 3})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.r.RunPublication(t.Context(), f.who, q[0].ID)
	wantCode(t, err, errcode.NotPublishable)
	q, err = f.r.PublicationRequests(t.Context(), f.who, f.v.AssetID)
	if err != nil || q[0].Status != "cancelled" {
		t.Fatal(q, err)
	}
}

func TestReviewPublicationRetryKeepsOriginalRequest(t *testing.T) {
	f := newReviewFixture(t)
	target := f.submit(t)
	if _, err := f.decide(t, target, ReviewDecision{Verdict: "approve"}); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	q, err := f.r.PublicationRequests(ctx, f.who, f.v.AssetID)
	if err != nil || len(q) != 1 {
		t.Fatal(q, err)
	}
	original := q[0]
	f.src.useErr = errcode.New(errcode.UseRestricted, "")
	_, err = f.r.RunPublication(ctx, f.who, original.ID)
	wantCode(t, err, errcode.UseRestricted)
	f.src.useErr = nil
	pub, err := f.r.RunPublication(ctx, f.who, original.ID)
	if err != nil || pub.PreviousRevision != original.ExpectedRevision {
		t.Fatal(pub, err)
	}
	q, err = f.r.PublicationRequests(ctx, f.who, f.v.AssetID)
	if err != nil || len(q) != 1 || q[0].ID != original.ID || q[0].ExpectedRevision != original.ExpectedRevision || q[0].Status != "succeeded" || q[0].FailureCode != "" {
		t.Fatal(q, err)
	}
	replay, err := f.r.RunPublication(ctx, f.who, original.ID)
	if err != nil || replay.ID != pub.ID {
		t.Fatal(replay, err)
	}
	var n int
	if err = f.db.QueryRowContext(ctx, `SELECT count(*) FROM ledger_publications WHERE asset_id=?`, f.v.AssetID).Scan(&n); err != nil || n != 1 {
		t.Fatal("retry duplicated publication history", n, err)
	}
}

func TestReviewNewApprovalPreservesPublicationAndAllowsAuditedRollback(t *testing.T) {
	f := newReviewFixture(t)
	f.src.profile.Profile.Defaults.Publication = "manual"
	ctx := t.Context()
	firstTarget := f.submit(t)
	firstReview, err := f.decide(t, firstTarget, ReviewDecision{Verdict: "approve"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := f.r.Publish(ctx, f.who, "publish-first", PublishRequest{ProjectID: f.project, VersionID: f.v.VersionID, ReviewID: firstReview.ID, Action: "publish", Reason: "first release"})
	if err != nil {
		t.Fatal(err)
	}
	req := f.req("second approved version")
	req.AssetID, req.Slug, req.BaseVersionID = f.v.AssetID, "", f.v.VersionID
	p, err := f.s.Prepare(ctx, f.command(commit.CommandType, f.project, "next-version", req), req)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := f.in.Install(ctx, p.InstallRequest())
	if err != nil {
		t.Fatal(err)
	}
	v, err := f.s.Commit(ctx, p.OperationID, f.who, proof)
	if err != nil {
		t.Fatal(err)
	}
	next := f.input
	next.VersionID, next.EvidenceIDs = v.VersionID, []ids.ID{}
	for _, old := range f.input.EvidenceIDs {
		e := f.src.evidence[old]
		e.ID, e.Ref, e.ManifestDigest = ids.New(), v.Ref(f.who.InstanceID), v.ManifestDigest
		check := *e.Check
		check.Ref, check.ManifestDigest = e.Ref, e.ManifestDigest
		e.Check = &check
		f.src.evidence[e.ID] = e
		next.EvidenceIDs = append(next.EvidenceIDs, e.ID)
	}
	secondTarget, err := f.r.Submit(ctx, f.who, "next-submit", next)
	if err != nil {
		t.Fatal(err)
	}
	secondReview, err := f.decide(t, secondTarget, ReviewDecision{Verdict: "approve"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := f.s.AssetControl(ctx, v.AssetID)
	if err != nil || a.PublishedVersionID != f.v.VersionID || a.PublicationRevision != first.Revision {
		t.Fatal("approval changed old publication", a, err)
	}
	// @approved follows the newest approval; @published stays on the release.
	wantAliases(t, f.s, v.AssetID, f.v.VersionID, v.VersionID)
	in := PublishRequest{ProjectID: f.project, VersionID: v.VersionID, ReviewID: secondReview.ID, ExpectedRevision: first.Revision, Action: "rollback", Reason: "cannot roll back to never published version"}
	_, err = f.r.Publish(ctx, f.who, "invalid-rollback", in)
	wantCode(t, err, errcode.NotPublishable)
	in.Action, in.Reason = "publish", "second release"
	second, err := f.r.Publish(ctx, f.who, "publish-second", in)
	if err != nil {
		t.Fatal(err)
	}
	wantAliases(t, f.s, v.AssetID, v.VersionID, v.VersionID)
	rollback, err := f.r.Publish(ctx, f.who, "rollback-first", PublishRequest{ProjectID: f.project, VersionID: f.v.VersionID, ReviewID: firstReview.ID, ExpectedRevision: second.Revision, Action: "rollback", Reason: "restore previous release"})
	if err != nil || rollback.FromVersionID != v.VersionID || rollback.ToVersionID != f.v.VersionID || rollback.Revision != 3 {
		t.Fatal(rollback, err)
	}
	if replay, err := f.r.Publish(ctx, f.who, "publish-second", in); err != nil || replay.ID != second.ID {
		t.Fatal(replay, err)
	}
	a, err = f.s.AssetControl(ctx, v.AssetID)
	if err != nil || a.PublishedVersionID != f.v.VersionID || a.PublicationRevision != rollback.Revision {
		t.Fatal("replayed release moved rollback pointer", a, err)
	}
	wantAliases(t, f.s, v.AssetID, f.v.VersionID, v.VersionID)
	rows, err := f.db.QueryContext(ctx, `SELECT record FROM ledger_publications WHERE asset_id=? ORDER BY revision`, v.AssetID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for _, want := range []Publication{first, second, rollback} {
		var raw string
		var got Publication
		if !rows.Next() {
			t.Fatal("missing publication history")
		}
		if err = rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal([]byte(raw), &got); err != nil || got != want {
			t.Fatal(got, want, err)
		}
	}
	if rows.Next() || rows.Err() != nil {
		t.Fatal("extra or unreadable history", rows.Err())
	}
}
func TestReviewTransactionFailureRollsBackAllEffects(t *testing.T) {
	f := newReviewFixture(t)
	target := f.submit(t)
	if _, err := f.db.ExecContext(t.Context(), `CREATE TRIGGER fail_request BEFORE INSERT ON ledger_publication_requests BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`); err != nil {
		t.Fatal(err)
	}
	_, err := f.decide(t, target, ReviewDecision{Verdict: "approve"})
	if err == nil {
		t.Fatal("injected fault succeeded")
	}
	state, _ := f.s.VersionControl(t.Context(), f.v.VersionID)
	if state.ReviewState != "submitted" || state.Revision != target.Revision {
		t.Fatal(state)
	}
	var count int
	if err = f.db.QueryRowContext(t.Context(), `SELECT count(*) FROM ledger_reviews`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

func TestReviewNewSubmissionAndCandidateGroups(t *testing.T) {
	for _, mode := range []string{"supersede", "candidates", "approved"} {
		t.Run(mode, func(t *testing.T) {
			f := newReviewFixture(t)
			if mode == "candidates" {
				f.flow.CandidateGroup = ids.New()
				f.input.Flow = f.flow
				for id, e := range f.src.evidence {
					e.Flow = f.flow
					f.src.evidence[id] = e
				}
			}
			target := f.submit(t)
			if mode == "approved" {
				if _, err := f.decide(t, target, ReviewDecision{Verdict: "approve"}); err != nil {
					t.Fatal(err)
				}
				requests, err := f.r.PublicationRequests(t.Context(), f.who, f.v.AssetID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.r.RunPublication(t.Context(), f.who, requests[0].ID); err != nil {
					t.Fatal(err)
				}
			}
			req := f.req("next bytes")
			req.AssetID = f.v.AssetID
			req.Slug = ""
			req.BaseVersionID = f.v.VersionID
			p, err := f.s.Prepare(t.Context(), f.command(commit.CommandType, f.project, "new-draft", req), req)
			if err != nil {
				t.Fatal(err)
			}
			proof, err := f.in.Install(t.Context(), p.InstallRequest())
			if err != nil {
				t.Fatal(err)
			}
			v, err := f.s.Commit(t.Context(), p.OperationID, f.who, proof)
			if err != nil {
				t.Fatal(err)
			}
			state, err := f.s.VersionControl(t.Context(), f.v.VersionID)
			if err != nil {
				t.Fatal(err)
			}
			want := "submitted"
			if mode == "approved" {
				want = "approved"
			}
			if state.ReviewState != want {
				t.Fatal("draft commit withdrew old review", state)
			}
			next := f.input
			next.VersionID = v.VersionID
			next.EvidenceIDs = []ids.ID{}
			for _, old := range f.input.EvidenceIDs {
				e := f.src.evidence[old]
				e.ID = ids.New()
				e.Ref = v.Ref(f.who.InstanceID)
				e.ManifestDigest = v.ManifestDigest
				check := *e.Check
				check.Ref = e.Ref
				check.ManifestDigest = e.ManifestDigest
				e.Check = &check
				f.src.evidence[e.ID] = e
				next.EvidenceIDs = append(next.EvidenceIDs, e.ID)
			}
			if _, err = f.r.Submit(t.Context(), f.who, "submit-next", next); err != nil {
				t.Fatal(err)
			}
			state, err = f.s.VersionControl(t.Context(), f.v.VersionID)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "supersede" {
				want = "withdrawn"
			}
			if state.ReviewState != want {
				t.Fatal(state)
			}
			if mode == "approved" {
				a, err := f.s.AssetControl(t.Context(), f.v.AssetID)
				if err != nil || a.PublishedVersionID != f.v.VersionID {
					t.Fatal(a, err)
				}
			}
		})
	}
}

func (s *reviewSourcesFixture) ProjectDocument(_ context.Context, _ authz.Context, ref ids.PermanentRef) (catalog.ProjectDocument, error) {
	d, ok := s.documents[ref]
	if !ok {
		return d, errcode.New(errcode.NotFound, "")
	}
	return d, nil
}
func TestReviewContextUsesCurrentApproval(t *testing.T) {
	f := newReviewFixture(t)
	ctx := t.Context()
	ref := f.v.Ref(f.who.InstanceID)
	f.src.documents = map[ids.PermanentRef]catalog.ProjectDocument{ref: {Contract: catalog.ProjectDocumentContract, Kind: "context", Title: "Rules", Scope: catalog.DocumentScope{Kind: "project"}, FilePath: "rules.md", Supersedes: []ids.PermanentRef{}}}
	list, err := f.r.EffectiveContext(ctx, f.who, f.project, manifest.TypeDoc)
	if err != nil || len(list) != 0 {
		t.Fatal(list, err)
	}
	target := f.submit(t)
	review, err := f.decide(t, target, ReviewDecision{Verdict: "approve"})
	if err != nil {
		t.Fatal(err)
	}
	list, err = f.r.EffectiveContext(ctx, f.who, f.project, manifest.TypeDoc)
	if err != nil || len(list) != 1 || list[0].ReviewID != review.ID {
		t.Fatal(list, err)
	}
	approval, err := f.r.ContextReview(ctx, f.who, ref)
	if err != nil || approval.ReviewID != review.ID {
		t.Fatal(approval, err)
	}
	// Archived approved documents remain exactly readable but leave the default
	// effective context. Test both the version and parent asset control.
	for _, scope := range []string{"version", "asset"} {
		if scope == "version" {
			_, err = f.db.ExecContext(ctx, `UPDATE ledger_version_states SET lifecycle='archived' WHERE version_id=?`, ref.VersionID)
		} else {
			_, err = f.db.ExecContext(ctx, `INSERT INTO ledger_asset_controls(asset_id,revision,lifecycle,publication_state,published_version_id,publication_revision,pending_operation_id) VALUES(?,1,'archived','unpublished','',0,'') ON CONFLICT(asset_id) DO UPDATE SET lifecycle='archived'`, ref.AssetID)
		}
		if err != nil {
			t.Fatal(err)
		}
		list, err = f.r.EffectiveContext(ctx, f.who, f.project, manifest.TypeDoc)
		if err != nil || len(list) != 0 {
			t.Fatal(scope, list, err)
		}
		if _, err = f.r.ContextReview(ctx, f.who, ref); err != nil {
			t.Fatal(scope, err)
		}
		if scope == "version" {
			_, err = f.db.ExecContext(ctx, `UPDATE ledger_version_states SET lifecycle='active' WHERE version_id=?`, ref.VersionID)
		} else {
			_, err = f.db.ExecContext(ctx, `UPDATE ledger_asset_controls SET lifecycle='active' WHERE asset_id=?`, ref.AssetID)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = f.decide(t, target, ReviewDecision{Action: identity.ActRevokeReview, Verdict: "revoke", EffectiveReviewID: review.ID, ExpectedRevision: 3})
	if err != nil {
		t.Fatal(err)
	}
	list, err = f.r.EffectiveContext(ctx, f.who, f.project, manifest.TypeDoc)
	if err != nil || len(list) != 0 {
		t.Fatal(list, err)
	}
	_, err = f.r.ContextReview(ctx, f.who, ref)
	wantCode(t, err, errcode.NotFound)
}

func TestReviewWaiverStableIdentityAndExpiryAtPublication(t *testing.T) {
	f := newReviewFixture(t)
	ctx := t.Context()
	f.src.profile.Profile.Defaults.Publication = "manual"
	req := f.src.profile.Profile.RequiredChecks[0]
	req.Key, req.Severity, req.Waivable = "visual", "warning", true
	f.src.profile.Profile.RequiredChecks = append(f.src.profile.Profile.RequiredChecks, req)
	f.src.profile.Profile.WaivableChecks = []string{"visual"}
	evidence := f.src.evidence[f.input.EvidenceIDs[0]]
	evidence.ID, evidence.CheckKey = ids.New(), "visual"
	check := *evidence.Check
	check.Verdict = "fail"
	evidence.Check = &check
	f.src.evidence[evidence.ID] = evidence
	f.input.EvidenceIDs = append(f.input.EvidenceIDs, evidence.ID)
	target := f.submit(t)
	decision := ReviewDecision{Verdict: "approve", Waivers: []ReviewWaiver{{CheckKey: "visual", Reason: "synthetic explicit waiver", EvidenceIDs: []ids.ID{evidence.ID}, ExpiresAt: clock.Format(f.clk.Now().Add(time.Minute))}}}
	review, err := f.decide(t, target, decision)
	if err != nil {
		t.Fatal(err)
	}
	if len(review.WaiverRecords) != 1 {
		t.Fatal(review)
	}
	waiver := review.WaiverRecords[0]
	expected, err := ids.DeriveChild(review.OperationID, "waiver:"+digest.Of([]byte("visual")).Hex())
	if err != nil || waiver.ID != expected || waiver.HumanGrantID != review.HumanGrantID || waiver.Scope != "review_target" || waiver.TargetID != target.ID || waiver.ManifestDigest != target.ManifestDigest {
		t.Fatal(waiver, err)
	}
	if err = validateReviewWaivers(review, target); err != nil {
		t.Fatal(err)
	}
	changed := review
	changed.WaiverRecords = append([]Waiver{}, review.WaiverRecords...)
	changed.WaiverRecords[0].VersionID = ids.New()
	if err = validateReviewWaivers(changed, target); errcode.CodeOf(err) != errcode.RefMismatch {
		t.Fatal("waiver transferred to another version", err)
	}
	f.clk.Advance(2 * time.Minute)
	_, err = f.r.Publish(ctx, f.who, "expired-waiver-publication", PublishRequest{ProjectID: f.project, VersionID: f.v.VersionID, ReviewID: review.ID, ExpectedRevision: 0, Action: "publish", Reason: "must reject expired waiver"})
	wantCode(t, err, errcode.ChecksNotSatisfied)
	stored, err := readJSON[Review](ctx, f.db, `SELECT record FROM ledger_reviews WHERE review_id=?`, review.ID)
	if err != nil || stored.WaiverRecords[0].ID != waiver.ID {
		t.Fatal(stored, err)
	}
}

// wantAliases checks the catalog selector port: an empty ID means the selector
// must report NOT_PUBLISHED (@published) or NOT_FOUND (@approved).
func wantAliases(t *testing.T, s *Service, asset, published, approved ids.ID) {
	t.Helper()
	got, err := s.Published(t.Context(), asset)
	if published == "" {
		wantCode(t, err, errcode.NotPublished)
	} else if err != nil || got != published {
		t.Fatal("@published", got, published, err)
	}
	got, err = s.Approved(t.Context(), asset)
	if approved == "" {
		wantCode(t, err, errcode.NotFound)
	} else if err != nil || got != approved {
		t.Fatal("@approved", got, approved, err)
	}
}

func publicationCount(t *testing.T, f *reviewFixture) int {
	t.Helper()
	var n int
	if err := f.db.QueryRowContext(t.Context(), `SELECT count(*) FROM ledger_publications WHERE asset_id=?`, f.v.AssetID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A replay that read the request as pending outside the lock must not be refused
// for lacking execution rights when, meanwhile, the same caller's other replay
// published it and the flow completed: the receipt is looked up under the lock
// before execution rights are required (BUG-20261001-06 review).
func TestReviewPublicationReplayAfterConcurrentCompletion(t *testing.T) {
	f := newReviewFixture(t)
	ctx := t.Context()
	target := f.submit(t)
	if _, err := f.decide(t, target, ReviewDecision{Verdict: "approve"}); err != nil {
		t.Fatal(err)
	}
	requests, err := f.r.PublicationRequests(ctx, f.who, f.v.AssetID)
	if err != nil || len(requests) != 1 || requests[0].Status != "pending" {
		t.Fatal(requests, err)
	}
	completed := false
	var other Publication
	f.src.taskHook = func(c context.Context) error {
		if completed {
			return errcode.New(errcode.InvalidStateTransition, "").WithDetails(errcode.Detail{Reason: "flow_completed"})
		}
		if commands.HoldsSecurity(c, f.s.gate.Coordinator()) {
			return nil // checked under the lock after the receipt lookup
		}
		// Checked before taking the lock: the other replay publishes and the
		// flow completes in between, as in the reviewed interleaving.
		f.src.taskHook = nil
		p, err := f.r.RunPublication(ctx, f.who, requests[0].ID)
		if err != nil {
			t.Error("concurrent publication", err)
		}
		other, completed = p, true
		f.src.taskErr = errcode.New(errcode.InvalidStateTransition, "").WithDetails(errcode.Detail{Reason: "flow_completed"})
		return f.src.taskErr
	}
	first, err := f.r.RunPublication(ctx, f.who, requests[0].ID)
	if err != nil {
		t.Fatal("replay refused although a receipt exists", err)
	}
	f.src.taskHook = nil
	f.src.taskErr = errcode.New(errcode.InvalidStateTransition, "").WithDetails(errcode.Detail{Reason: "flow_completed"})
	again, err := f.r.RunPublication(ctx, f.who, requests[0].ID)
	if err != nil || again != first || (other != Publication{} && other != first) {
		t.Fatal("both callers must see the one receipt", first, again, other, err)
	}
	if n := publicationCount(t, f); n != 1 {
		t.Fatal("one publication expected", n)
	}
}

// Only a current flow executor may record queue failure diagnostics; a refused
// caller leaves the request untouched.
func TestReviewPublicationDiagnosticsRequireExecutor(t *testing.T) {
	f := newReviewFixture(t)
	ctx := t.Context()
	target := f.submit(t)
	if _, err := f.decide(t, target, ReviewDecision{Verdict: "approve"}); err != nil {
		t.Fatal(err)
	}
	requests, err := f.r.PublicationRequests(ctx, f.who, f.v.AssetID)
	if err != nil || len(requests) != 1 {
		t.Fatal(requests, err)
	}
	f.src.taskErr = errcode.New(errcode.Forbidden, "")
	_, err = f.r.RunPublication(ctx, f.who, requests[0].ID)
	wantCode(t, err, errcode.Forbidden)
	if requests, err = f.r.PublicationRequests(ctx, f.who, f.v.AssetID); err != nil || requests[0].Status != "pending" || requests[0].FailureCode != "" {
		t.Fatal("a non-executor wrote diagnostics", requests, err)
	}
	f.src.taskErr = nil
	f.src.useErr = errcode.New(errcode.UseRestricted, "")
	_, err = f.r.RunPublication(ctx, f.who, requests[0].ID)
	wantCode(t, err, errcode.UseRestricted)
	if requests, err = f.r.PublicationRequests(ctx, f.who, f.v.AssetID); err != nil || requests[0].Status != "failed" || requests[0].FailureCode != "USE_RESTRICTED" {
		t.Fatal("the executor's failure must be recorded", requests, err)
	}
}
