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
func (s *reviewSourcesFixture) Task(context.Context, authz.Context, commit.Committed, ReviewFlow, string) error {
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
	_, err = f.decide(t, target, ReviewDecision{Action: identity.ActRevokeReview, ExpectedRevision: 3, EffectiveReviewID: review.ID, Verdict: "revoke"})
	if err != nil {
		t.Fatal(err)
	}
	// A completed old operation may return its historical receipt, but cannot
	// update the publication pointer after the same-transaction suspension.
	if _, err = f.r.RunPublication(t.Context(), f.who, requests[0].ID); err != nil {
		t.Fatal(err)
	}
	a, err = f.s.AssetControl(t.Context(), f.v.AssetID)
	if err != nil || a.PublicationState != "suspended" || a.PublishedVersionID != "" || a.PublicationRevision != 2 {
		t.Fatal(a, err)
	}
	state, _ := f.s.VersionControl(t.Context(), f.v.VersionID)
	if state.ReviewState != "withdrawn" || state.EffectiveReviewID != "" {
		t.Fatal(state)
	}
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
	in := PublishRequest{ProjectID: f.project, VersionID: v.VersionID, ReviewID: secondReview.ID, ExpectedRevision: first.Revision, Action: "rollback", Reason: "cannot roll back to never published version"}
	_, err = f.r.Publish(ctx, f.who, "invalid-rollback", in)
	wantCode(t, err, errcode.NotPublishable)
	in.Action, in.Reason = "publish", "second release"
	second, err := f.r.Publish(ctx, f.who, "publish-second", in)
	if err != nil {
		t.Fatal(err)
	}
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
