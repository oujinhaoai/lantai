package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
	"github.com/oujinhaoai/lantai/internal/extensions"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
)

// CheckRequirement pins both the extension package and configuration. Matching a
// processor name alone never establishes compatibility. Result is the shared
// lantai.check-result/v1 protocol, not a second validator protocol.
type CheckRequirement struct {
	Key                string             `json:"check_key"`
	SchemaVersion      int                `json:"schema_version"`
	AcceptedProcessors []storage.Producer `json:"accepted_processor_versions"`
	ConfigDigest       digest.Digest      `json:"config_digest"`
	Severity           string             `json:"severity"`
	Waivable           bool               `json:"waivable"`
}
type AcceptanceProfile struct {
	Contract          string               `json:"contract"`
	ID                string               `json:"profile_id"`
	Revision          int64                `json:"revision"`
	AssetTypes        []manifest.AssetType `json:"asset_types"`
	Purpose           authz.Purpose        `json:"purpose"`
	RequiredChecks    []CheckRequirement   `json:"required_checks"`
	RequiredEvidence  []string             `json:"required_evidence"`
	QARequired        bool                 `json:"qa_required"`
	DistinctActorRule string               `json:"distinct_actor_rule"`
	WaivableChecks    []string             `json:"waivable_checks"`
	Defaults          ProfileDefaults      `json:"defaults"`
}
type ProfileDefaults struct {
	Publication string `json:"publication"`
}
type ProfileSnapshot struct {
	Ref            ids.PermanentRef  `json:"ref"`
	ManifestDigest digest.Digest     `json:"manifest_digest"`
	Profile        AcceptanceProfile `json:"profile"`
}

// ReviewFlow names T05's live task attempt. T03 never owns or increments fences.
type ReviewFlow struct {
	TaskID         ids.ID `json:"task_id"`
	AttemptID      ids.ID `json:"attempt_id"`
	Round          int64  `json:"round"`
	Fence          int64  `json:"fence"`
	CandidateGroup ids.ID `json:"candidate_group,omitempty"`
}

// AcceptedEvidence is returned only by a server-owned adapter which has verified
// T06 completion/producer provenance and the immutable file's hash. Mere file
// presence or a worker-supplied result is not acceptance.
type AcceptedEvidence struct {
	QAVerdict      string                  `json:"qa_verdict,omitempty"`
	ID             ids.ID                  `json:"evidence_id"`
	Digest         digest.Digest           `json:"digest"`
	Ref            ids.PermanentRef        `json:"ref"`
	ManifestDigest digest.Digest           `json:"manifest_digest"`
	Kind           string                  `json:"kind"`
	ActorID        ids.ID                  `json:"actor_id"`
	Flow           ReviewFlow              `json:"flow,omitzero"`
	CheckKey       string                  `json:"check_key,omitempty"`
	SchemaVersion  int                     `json:"schema_version,omitempty"`
	ConfigDigest   digest.Digest           `json:"config_digest,omitempty"`
	Check          *extensions.CheckResult `json:"check,omitempty"`
	CompletedAt    string                  `json:"completed_at"`
}

// ReviewSources are read-only ports called under the shared security/project
// guard, outside any ledger transaction. They must not reacquire that guard.
// Task validates CURRENT task/round/fence, maker, candidate group, and assignee
// for submit/review/publish. Evidence must be accepted, completed, and current.
// Use applies current rights, including inherited/personal restrictions.
type ReviewSources interface {
	Profile(context.Context, authz.Context, ids.PermanentRef) (ProfileSnapshot, error)
	Evidence(context.Context, authz.Context, ids.ID) (AcceptedEvidence, error)
	Task(context.Context, authz.Context, commit.Committed, ReviewFlow, string) error
	Use(context.Context, authz.Context, commit.Committed, authz.Purpose) error
	AssetType(context.Context, commit.Committed) (manifest.AssetType, error)
}
type ReviewPolicies interface {
	ResolvePolicies(context.Context, ids.ID) (map[string]identity.PolicyValue, error)
}
type Reviews struct {
	ledger   *Service
	sources  ReviewSources
	policies ReviewPolicies
}

func (s *Service) NewReviews(source ReviewSources, policy ReviewPolicies) (*Reviews, error) {
	if source == nil || policy == nil {
		return nil, errors.New("ledger: review sources and current identity policies required")
	}
	return &Reviews{s, source, policy}, nil
}

type SubmitReview struct {
	InitialProfile   bool             `json:"initial_profile,omitempty"`
	ProjectID        ids.ID           `json:"project_id"`
	VersionID        ids.ID           `json:"version_id"`
	ExpectedRevision int64            `json:"expected_revision"`
	ProfileRef       ids.PermanentRef `json:"profile_ref"`
	EvidenceIDs      []ids.ID         `json:"evidence_ids"`
	Flow             ReviewFlow       `json:"flow"`
}
type ReviewTarget struct {
	ID ids.ID `json:"target_id"`
	SubmitReview
	AssetID        ids.ID             `json:"asset_id"`
	ManifestDigest digest.Digest      `json:"manifest_digest"`
	Profile        ProfileSnapshot    `json:"profile"`
	Evidence       []AcceptedEvidence `json:"evidence"`
	Revision       int64              `json:"target_revision"`
	MakerID        ids.ID             `json:"maker_id"`
	SubmittedBy    ids.ID             `json:"submitted_by"`
	CreatedAt      string             `json:"created_at"`
}

func (r *Reviews) Target(ctx context.Context, who authz.Context, id ids.ID) (ReviewTarget, error) {
	ctx, held, err := r.ledger.gate.Coordinator().Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return ReviewTarget{}, err
	}
	defer held.Release()
	return r.visibleTarget(ctx, who, id)
}
func (r *Reviews) visibleTarget(ctx context.Context, who authz.Context, id ids.ID) (ReviewTarget, error) {
	t, err := r.target(ctx, id)
	if err != nil {
		return t, err
	}
	if err = r.ledger.authorize(ctx, who, "catalog.read", t.ProjectID, "version", t.VersionID); err != nil {
		return ReviewTarget{}, err
	}
	v, err := r.ledger.VersionByID(ctx, t.VersionID)
	if err != nil {
		return ReviewTarget{}, err
	}
	if err = r.ledger.CheckVersionRead(ctx, t.AssetID, t.VersionID); err != nil {
		return ReviewTarget{}, err
	}
	if err = r.sources.Use(ctx, who, v, authz.PurposeArchiveReview); err != nil {
		return ReviewTarget{}, err
	}
	return t, nil
}
func (r *Reviews) target(ctx context.Context, id ids.ID) (ReviewTarget, error) {
	return readJSON[ReviewTarget](ctx, r.ledger.db, `SELECT record FROM ledger_review_targets WHERE target_id=?`, id)
}
func nonwaivable(key string) bool {
	return slices.Contains([]string{"integrity", "schema", "identity", "target", "current_use", "license_evidence", "purpose"}, key)
}
func validateProfile(p AcceptanceProfile) error {
	raw, err := canonjson.CanonicalizeValue(p)
	if err != nil {
		return err
	}
	reg, err := schema.Default()
	if err != nil {
		return err
	}
	if err = reg.ValidateJSON("lantai.acceptance-profile/v1", raw); err != nil {
		return errcode.Wrap(errcode.SchemaInvalid, "invalid acceptance profile", err)
	}

	if p.Contract != "lantai.acceptance-profile/v1" || strings.TrimSpace(p.ID) == "" || p.Revision < 1 || len(p.AssetTypes) == 0 || len(p.RequiredChecks) > 100 || len(p.RequiredEvidence) > 100 || !slices.Contains([]string{"maker_checker", "maker_checker_reviewer"}, p.DistinctActorRule) || !slices.Contains([]string{"auto", "manual"}, p.Defaults.Publication) {
		return invalid("invalid acceptance profile")
	}
	if !slices.Contains([]authz.Purpose{authz.PurposeArchiveReview, authz.PurposeReference, authz.PurposeProduction, authz.PurposeGenerativeInput, authz.PurposeRawExport}, p.Purpose) {
		return invalid("invalid acceptance purpose")
	}
	for _, a := range p.AssetTypes {
		if !a.Valid() {
			return invalid("unknown asset type")
		}
	}
	seen := map[string]bool{}
	for _, c := range p.RequiredChecks {
		if c.Key == "" || seen[c.Key] || c.SchemaVersion != 1 || !c.ConfigDigest.Valid() || len(c.AcceptedProcessors) == 0 || len(c.AcceptedProcessors) > 100 || !slices.Contains([]string{"error", "warning"}, c.Severity) {
			return invalid("invalid check requirement")
		}
		seen[c.Key] = true
		if c.Waivable != slices.Contains(p.WaivableChecks, c.Key) || c.Waivable && nonwaivable(c.Key) {
			return invalid("invalid waiver policy")
		}
		for _, p := range c.AcceptedProcessors {
			if p.ExtensionID == "" || p.ExtensionVersion == "" || !p.PackageDigest.Valid() || p.ContributionID == "" || !slices.Contains([]string{"builtin_release", "package"}, p.Source) {
				return invalid("exact producer identity required")
			}
		}
	}
	for _, key := range []string{"integrity", "schema", "license_evidence", "purpose"} {
		if !seen[key] {
			return invalid("basic acceptance check missing")
		}
	}
	for _, key := range p.WaivableChecks {
		if !seen[key] {
			return invalid("unknown waivable check")
		}
	}
	return nil
}
func (r *Reviews) profile(ctx context.Context, who authz.Context, ref ids.PermanentRef) (ProfileSnapshot, error) {
	return r.profileSnapshot(ctx, who, ref, true)
}
func (r *Reviews) profileSnapshot(ctx context.Context, who authz.Context, ref ids.PermanentRef, approved bool) (ProfileSnapshot, error) {
	p, err := r.sources.Profile(ctx, who, ref)
	if err != nil {
		return p, err
	}
	if p.Ref != ref || ref.InstanceID != who.InstanceID || !ref.AssetID.Valid() || !ref.VersionID.Valid() {
		return p, errcode.New(errcode.RefMismatch, "")
	}
	v, err := r.ledger.Version(ctx, ref.AssetID, ref.VersionID)
	if err != nil {
		return p, err
	}
	if v.ManifestDigest != p.ManifestDigest {
		return p, errcode.New(errcode.HashMismatch, "")
	}
	if err = r.ledger.CheckVersionRead(ctx, ref.AssetID, ref.VersionID); err != nil {
		return p, err
	}
	state, err := r.ledger.VersionControl(ctx, ref.VersionID)
	if err != nil {
		return p, err
	}
	if approved && (state.ReviewState != "approved" || !state.EffectiveReviewID.Valid()) {
		return p, errcode.New(errcode.ChecksNotSatisfied, "profile is not currently approved")
	}
	typ, err := r.sources.AssetType(ctx, v)
	if err != nil {
		return p, err
	}
	if typ != manifest.TypeConfig {
		return p, invalid("profile must be a configuration asset")
	}
	return p, validateProfile(p.Profile)
}
func (s *Service) commandContext(ctx context.Context, who authz.Context, key, typ string, project ids.ID, body any) (commands.Context, error) {
	raw, err := canonjson.CanonicalizeValue(body)
	if err != nil {
		return commands.Context{}, err
	}
	if len(raw) > 48<<10 {
		return commands.Context{}, invalid("command request too large")
	}
	hash, err := commands.RequestHash(commands.HashInput{CommandType: typ, ProjectID: project, Body: raw})
	if err != nil {
		return commands.Context{}, err
	}
	epoch, err := s.authority.RecoveryEpoch(ctx)
	if err != nil {
		return commands.Context{}, err
	}
	id, err := s.ids.New()
	if err != nil {
		return commands.Context{}, err
	}
	return commands.Context{OperationID: id, IdempotencyKey: key, CommandType: typ, ActorID: who.PrincipalID, SessionID: who.SessionID, ProjectID: project, RequestHash: hash, RecoveryEpoch: epoch, PolicyRevision: who.PolicyRevision}, nil
}
func (r *Reviews) Submit(ctx context.Context, who authz.Context, key string, in SubmitReview) (ReviewTarget, error) {
	var out ReviewTarget
	s := r.ledger
	if !in.ProjectID.Valid() || !in.VersionID.Valid() || in.ExpectedRevision < 1 || len(in.EvidenceIDs) > 100 || !in.Flow.TaskID.Valid() || !in.Flow.AttemptID.Valid() || in.Flow.Round < 1 || in.Flow.Fence < 1 || in.Flow.CandidateGroup != "" && !in.Flow.CandidateGroup.Valid() {
		return out, invalid("fixed version, revision, evidence and flow required")
	}
	ctx, release, err := s.write(ctx, in.ProjectID)
	if err != nil {
		return out, err
	}
	defer release()
	if err = s.authorize(ctx, who, "ledger.submit_review", in.ProjectID, "version", in.VersionID); err != nil {
		return out, err
	}
	v, err := s.VersionByID(ctx, in.VersionID)
	if err != nil {
		return out, err
	}
	if v.ProjectID != in.ProjectID {
		return out, errcode.New(errcode.RefMismatch, "")
	}
	if err = s.CheckVersionRead(ctx, v.AssetID, v.VersionID); err != nil {
		return out, err
	}
	if err = r.sources.Use(ctx, who, v, authz.PurposeArchiveReview); err != nil {
		return out, err
	}
	cmd, err := s.commandContext(ctx, who, key, "ledger.submit_review", in.ProjectID, in)
	if err != nil {
		return out, err
	}
	receipt, err := s.lookup(ctx, s.db, cmd)
	if err != nil {
		return out, err
	}
	if receipt != nil {
		if commands.Decide(receipt, cmd.RequestHash, s.clock.Now()) == commands.OutcomeExpired {
			return out, errcode.New(errcode.IdempotencyResultExpired, "")
		}
		err = json.Unmarshal(receipt.ResponseSummary, &out)
		return out, err
	}
	if err = s.CheckAssetWrite(ctx, v.AssetID); err != nil {
		return out, err
	}
	state, err := s.VersionControl(ctx, v.VersionID)
	if err != nil {
		return out, err
	}
	if state.Revision != in.ExpectedRevision {
		return out, errcode.New(errcode.ReviewTargetStale, "")
	}
	if !slices.Contains([]string{"draft", "withdrawn"}, state.ReviewState) || state.Lifecycle != "active" {
		return out, errcode.New(errcode.NotSubmittable, "")
	}
	if err = r.sources.Task(ctx, who, v, in.Flow, "submit"); err != nil {
		return out, err
	}
	var profile ProfileSnapshot
	if in.InitialProfile {
		if in.ProfileRef != v.Ref(who.InstanceID) {
			return out, invalid("initial profile must be the exact configuration under review")
		}
		profile, err = r.initialProfile(ctx, who, in.ProjectID, in.ProfileRef)
	} else {
		profile, err = r.profile(ctx, who, in.ProfileRef)
	}
	if err != nil {
		return out, err
	}
	typ, err := r.sources.AssetType(ctx, v)
	if err != nil {
		return out, err
	}
	if !slices.Contains(profile.Profile.AssetTypes, typ) {
		return out, errcode.New(errcode.ChecksNotSatisfied, "profile asset type differs")
	}
	evidence, err := r.evidence(ctx, who, v, in.Flow, in.EvidenceIDs)
	if err != nil {
		return out, err
	}
	id, err := s.ids.New()
	if err != nil {
		return out, err
	}
	out = ReviewTarget{ID: id, SubmitReview: in, AssetID: v.AssetID, ManifestDigest: v.ManifestDigest, Profile: profile, Evidence: evidence, Revision: state.Revision + 1, MakerID: v.CommittedBy, SubmittedBy: who.PrincipalID, CreatedAt: clock.Format(s.clock.Now())}
	if out.EvidenceIDs == nil {
		out.EvidenceIDs = []ids.ID{}
	}
	if err = validateReviewContract("lantai.review-target/v1", out); err != nil {
		return ReviewTarget{}, err
	}
	if raw, err := canonjson.CanonicalizeValue(out); err != nil || len(raw) > 48<<10 {
		return ReviewTarget{}, invalid("review target exceeds receipt size")
	}
	result, err := s.store.Execute(ctx, s.db, cmd, func(ctx context.Context, tx *sql.Tx) (commands.Result, error) {
		var events []event.Envelope
		rows, err := tx.QueryContext(ctx, `SELECT v.version_id,t.record FROM ledger_version_states v JOIN ledger_review_targets t ON t.target_id=v.review_target_id WHERE t.asset_id=? AND v.state='submitted'`, v.AssetID)
		if err != nil {
			return commands.Result{}, err
		}
		var old []ReviewTarget
		for rows.Next() {
			var id ids.ID
			var raw string
			if err = rows.Scan(&id, &raw); err != nil {
				rows.Close()
				return commands.Result{}, err
			}
			var t ReviewTarget
			if err = json.Unmarshal([]byte(raw), &t); err != nil {
				rows.Close()
				return commands.Result{}, err
			}
			old = append(old, t)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return commands.Result{}, err
		}
		for _, t := range old {
			if in.Flow.CandidateGroup != "" && t.Flow.CandidateGroup == in.Flow.CandidateGroup && t.Flow.TaskID == in.Flow.TaskID && t.Flow.Round == in.Flow.Round {
				continue
			}
			st, err := versionControl(ctx, tx, t.VersionID)
			if err != nil {
				return commands.Result{}, err
			}
			st.ReviewState = "withdrawn"
			st.WithdrawalReason = "superseded_submission"
			st.Revision++
			if err = saveVersionControl(ctx, tx, st); err != nil {
				return commands.Result{}, err
			}
			e, err := s.event(cmd, "review.withdrawn", "version", st.VersionID, st.Revision, map[string]any{"target_id": t.ID, "reason": st.WithdrawalReason})
			if err != nil {
				return commands.Result{}, err
			}
			events = append(events, e)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO ledger_review_targets VALUES(?,?,?,?,?,?)`, id, v.ProjectID, v.AssetID, v.VersionID, in.Flow.CandidateGroup, encoded(out)); err != nil {
			return commands.Result{}, err
		}
		state.ReviewState = "submitted"
		state.ReviewTargetID = id
		state.EffectiveReviewID = ""
		state.WithdrawalReason = ""
		state.Revision++
		if err = saveVersionControl(ctx, tx, state); err != nil {
			return commands.Result{}, err
		}
		e, err := s.event(cmd, "review.submitted", "version", v.VersionID, state.Revision, map[string]any{"target_id": id})
		if err != nil {
			return commands.Result{}, err
		}
		events = append(events, e)
		return commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 201, Summary: out, Events: events}, nil
	})
	if err != nil {
		return ReviewTarget{}, err
	}
	err = json.Unmarshal(result.Receipt.ResponseSummary, &out)
	return out, err
}
func (r *Reviews) evidence(ctx context.Context, who authz.Context, v commit.Committed, flow ReviewFlow, list []ids.ID) ([]AcceptedEvidence, error) {
	out := make([]AcceptedEvidence, 0, len(list))
	seen := map[ids.ID]bool{}
	for _, id := range list {
		if !id.Valid() || seen[id] {
			return nil, invalid("duplicate or invalid evidence")
		}
		seen[id] = true
		e, err := r.sources.Evidence(ctx, who, id)
		if err != nil {
			return nil, err
		}
		at, parseErr := clock.Parse(e.CompletedAt)
		if e.ID != id || !e.Digest.Valid() || e.Ref != v.Ref(who.InstanceID) || e.ManifestDigest != v.ManifestDigest || !e.ActorID.Valid() || ((e.Kind == "check_result" || e.Kind == "qa_report") && e.Flow != flow) || parseErr != nil || at.After(r.ledger.clock.Now()) {
			return nil, errcode.New(errcode.ChecksNotSatisfied, "evidence target, round or completion differs")
		}
		if e.Kind == "check_result" && (e.Check == nil || e.Check.Ref != e.Ref || e.Check.ManifestDigest != e.ManifestDigest || e.Check.Contract != "lantai.check-result/v1") {
			return nil, errcode.New(errcode.ChecksNotSatisfied, "check result does not match envelope")
		}
		out = append(out, e)
	}
	return out, nil
}

type ReviewWaiver struct {
	CheckKey    string   `json:"check_key"`
	Reason      string   `json:"reason"`
	EvidenceIDs []ids.ID `json:"evidence_ids"`
	ExpiresAt   string   `json:"expires_at,omitempty"`
}

// Waiver is an immutable acceptance record stored inside its owning Review.
// Its ID derives from the review operation, so replay cannot mint another grant.
type Waiver struct {
	ID ids.ID `json:"waiver_id"`
	ReviewWaiver
	VersionID      ids.ID        `json:"version_id"`
	ManifestDigest digest.Digest `json:"manifest_digest"`
	TargetID       ids.ID        `json:"target_id"`
	ReviewID       ids.ID        `json:"review_id"`
	ApproverID     ids.ID        `json:"approver_id"`
	HumanGrantID   ids.ID        `json:"human_grant_id"`
	OperationID    ids.ID        `json:"operation_id"`
	CreatedAt      string        `json:"created_at"`
	Scope          string        `json:"scope"`
}
type ReviewDecision struct {
	Action            authz.Action   `json:"action"`
	TargetID          ids.ID         `json:"target_id"`
	ExpectedRevision  int64          `json:"expected_revision"`
	EffectiveReviewID ids.ID         `json:"effective_review_id,omitempty"`
	Verdict           string         `json:"verdict"`
	Reason            string         `json:"reason"`
	Waivers           []ReviewWaiver `json:"waivers"`
}
type Review struct {
	ID            ids.ID   `json:"review_id"`
	WaiverRecords []Waiver `json:"waiver_records"`
	ReviewDecision
	VersionID          ids.ID        `json:"version_id"`
	ManifestDigest     digest.Digest `json:"manifest_digest"`
	ProfileDigest      digest.Digest `json:"profile_digest"`
	ActorID            ids.ID        `json:"actor_id"`
	HumanGrantID       ids.ID        `json:"human_grant_id"`
	OperationID        ids.ID        `json:"operation_id"`
	CreatedAt          string        `json:"created_at"`
	SelfReview         bool          `json:"self_review"`
	PublicationPending bool          `json:"publication_pending"`
}

func (r *Reviews) HumanAction(ctx context.Context, in ReviewDecision) (identity.HumanAction, error) {
	if in.ExpectedRevision < 1 || strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 4096 || len(in.Waivers) > 100 {
		return identity.HumanAction{}, invalid("review revision and reason required")
	}
	if in.Action == identity.ActRecordReview {
		if !slices.Contains([]string{"approve", "return", "reject"}, in.Verdict) || in.EffectiveReviewID != "" {
			return identity.HumanAction{}, invalid("invalid review decision")
		}
	} else if in.Action == identity.ActRevokeReview {
		if in.Verdict != "revoke" || !in.EffectiveReviewID.Valid() || len(in.Waivers) > 0 {
			return identity.HumanAction{}, invalid("exact current review required")
		}
	} else {
		return identity.HumanAction{}, invalid("invalid review action")
	}
	t, err := r.target(ctx, in.TargetID)
	if err != nil {
		return identity.HumanAction{}, err
	}
	raw, err := canonjson.CanonicalizeValue(in)
	if err != nil {
		return identity.HumanAction{}, err
	}
	return identity.HumanAction{Action: in.Action, ProjectID: t.ProjectID, ResourceID: t.ID, ResourceRevision: in.ExpectedRevision, ManifestDigest: t.ManifestDigest, Request: raw}, nil
}
func (r *Reviews) ValidateHumanTarget(ctx context.Context, who authz.Context, a identity.HumanAction) error {
	if a.Action != identity.ActRecordReview && a.Action != identity.ActRevokeReview {
		return r.ledger.ValidateHumanTarget(ctx, who, a)
	}
	var in ReviewDecision
	if err := json.Unmarshal(a.Request, &in); err != nil {
		return err
	}
	want, err := r.HumanAction(ctx, in)
	if err != nil {
		return err
	}
	x, err := canonjson.CanonicalizeValue(a)
	if err != nil {
		return err
	}
	y, err := canonjson.CanonicalizeValue(want)
	if err != nil {
		return err
	}
	if string(x) != string(y) {
		return errcode.New(errcode.HumanGrantMismatch, "")
	}
	_, err = r.visibleTarget(ctx, who, in.TargetID)
	return err
}
func (r *Reviews) Record(ctx context.Context, who authz.Context, in ReviewDecision, grant, child ids.ID, human HumanCommands) (commands.Receipt, error) {
	if human == nil {
		return commands.Receipt{}, errcode.New(errcode.HumanProofRequired, "")
	}
	a, err := r.HumanAction(ctx, in)
	if err != nil {
		return commands.Receipt{}, err
	}
	return human.AcceptHumanItem(ctx, who, grant, child, a, commands.Request{Security: commands.ModeExclusive, Projects: []string{string(a.ProjectID)}}, &reviewCommand{r: r, who: who, in: in})
}

type reviewCommand struct {
	r   *Reviews
	who authz.Context
	in  ReviewDecision
}

func (c *reviewCommand) Receipt(ctx context.Context, id ids.ID) (commands.Receipt, error) {
	v, err := c.r.ledger.store.ReceiptByOperation(ctx, c.r.ledger.db, id)
	if err != nil {
		return commands.Receipt{}, err
	}
	return *v, nil
}
func policyBool(p map[string]identity.PolicyValue, key string) bool {
	var value bool
	_ = json.Unmarshal(p[key].Value, &value)
	return value
}
func (c *reviewCommand) Commit(ctx context.Context, cmd commands.Context, _ identity.HumanAction) (commands.Receipt, error) {
	r, s, in := c.r, c.r.ledger, c.in
	t, err := r.target(ctx, in.TargetID)
	if err != nil {
		return commands.Receipt{}, err
	}
	v, err := s.VersionByID(ctx, t.VersionID)
	if err != nil {
		return commands.Receipt{}, err
	}
	state, err := s.VersionControl(ctx, t.VersionID)
	if err != nil {
		return commands.Receipt{}, err
	}
	if state.Revision != in.ExpectedRevision || state.ReviewTargetID != t.ID || v.ManifestDigest != t.ManifestDigest {
		return commands.Receipt{}, errcode.New(errcode.ReviewTargetStale, "")
	}
	if t.InitialProfile {
		if err = s.authorize(ctx, c.who, "ledger.initialize_profile", t.ProjectID, "version", t.VersionID); err != nil {
			return commands.Receipt{}, err
		}
	}
	var self bool
	if in.Action == identity.ActRevokeReview {
		if state.ReviewState != "approved" || state.EffectiveReviewID != in.EffectiveReviewID {
			return commands.Receipt{}, errcode.New(errcode.ReviewTargetStale, "")
		}
		old, err := readJSON[Review](ctx, s.db, `SELECT record FROM ledger_reviews WHERE review_id=?`, in.EffectiveReviewID)
		if err != nil {
			return commands.Receipt{}, err
		}
		if old.ActorID != c.who.PrincipalID {
			if err = s.authorize(ctx, c.who, "ledger.publish", t.ProjectID, "asset", t.AssetID); err != nil {
				return commands.Receipt{}, err
			}
		}
		// Risk revocation can bypass an ordinary content lock or availability ban.
		if err = s.CheckEvidenceAppend(ctx, t.AssetID, t.VersionID); err != nil {
			return commands.Receipt{}, err
		}
	} else {
		if state.ReviewState != "submitted" || state.Lifecycle != "active" {
			return commands.Receipt{}, errcode.New(errcode.ReviewTargetStale, "")
		}
		if err = s.CheckVersionRead(ctx, t.AssetID, t.VersionID); err != nil {
			return commands.Receipt{}, err
		}
		if err = s.CheckAssetWrite(ctx, t.AssetID); err != nil {
			return commands.Receipt{}, err
		}
		if err = r.sources.Task(ctx, c.who, v, t.Flow, "review"); err != nil {
			return commands.Receipt{}, err
		}
		if err = r.sources.Use(ctx, c.who, v, authz.PurposeArchiveReview); err != nil {
			return commands.Receipt{}, err
		}
		policies, err := r.policies.ResolvePolicies(ctx, t.ProjectID)
		if err != nil {
			return commands.Receipt{}, err
		}
		self = t.MakerID == c.who.PrincipalID
		if self && !policyBool(policies, "review.allow_self_human") {
			return commands.Receipt{}, errcode.New(errcode.SelfReviewForbidden, "")
		}
		if in.Verdict == "approve" {
			if err = r.satisfied(ctx, c.who, v, t, in.Waivers, policyBool(policies, "review.qa_required"), c.who.PrincipalID); err != nil {
				return commands.Receipt{}, err
			}
		} else if len(in.Waivers) > 0 {
			return commands.Receipt{}, invalid("waivers apply only to approval")
		}
	}
	id, err := s.ids.New()
	if err != nil {
		return commands.Receipt{}, err
	}
	out := Review{ID: id, ReviewDecision: in, VersionID: v.VersionID, ManifestDigest: v.ManifestDigest, ProfileDigest: t.Profile.ManifestDigest, ActorID: cmd.ActorID, HumanGrantID: cmd.HumanGrantID, OperationID: cmd.OperationID, CreatedAt: clock.Format(s.clock.Now()), SelfReview: self, PublicationPending: in.Verdict == "approve" && t.Profile.Profile.Defaults.Publication == "auto"}
	if out.Waivers == nil {
		out.Waivers = []ReviewWaiver{}
	}
	out.WaiverRecords = []Waiver{}
	for _, w := range in.Waivers {
		waiverID, err := ids.DeriveChild(cmd.OperationID, "waiver:"+digest.Of([]byte(w.CheckKey)).Hex())
		if err != nil {
			return commands.Receipt{}, err
		}
		out.WaiverRecords = append(out.WaiverRecords, Waiver{ID: waiverID, ReviewWaiver: w, VersionID: v.VersionID, ManifestDigest: v.ManifestDigest, TargetID: t.ID, ReviewID: id, ApproverID: cmd.ActorID, HumanGrantID: cmd.HumanGrantID, OperationID: cmd.OperationID, CreatedAt: out.CreatedAt, Scope: "review_target"})
	}
	if err = validateReviewWaivers(out, t); err != nil {
		return commands.Receipt{}, err
	}
	result, err := s.store.Execute(ctx, s.db, cmd, func(ctx context.Context, tx *sql.Tx) (commands.Result, error) {
		switch in.Verdict {
		case "approve":
			state.ReviewState = "approved"
			state.EffectiveReviewID = id
		case "return":
			state.ReviewState = "changes_requested"
		case "reject":
			state.ReviewState = "rejected"
		case "revoke":
			state.ReviewState = "withdrawn"
			state.EffectiveReviewID = ""
			state.WithdrawalReason = "review_revoked"
		}
		state.Revision++
		if err := saveVersionControl(ctx, tx, state); err != nil {
			return commands.Result{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ledger_reviews VALUES(?,?,?,?,?)`, id, t.ID, v.VersionID, cmd.OperationID, encoded(out)); err != nil {
			return commands.Result{}, err
		}
		ev, err := s.event(cmd, "review.recorded", "version", v.VersionID, state.Revision, map[string]any{"review_id": id, "target_id": t.ID, "verdict": in.Verdict})
		if err != nil {
			return commands.Result{}, err
		}
		events := []event.Envelope{ev}
		if in.Verdict == "revoke" {
			e, err := s.suspendVersion(ctx, tx, cmd, v.AssetID, v.VersionID, in.Reason)
			if err != nil {
				return commands.Result{}, err
			}
			if e != nil {
				events = append(events, *e)
			}
			if _, err = tx.ExecContext(ctx, `UPDATE ledger_publication_requests SET status='cancelled',failure_code='REVIEW_TARGET_STALE' WHERE review_id=? AND status IN ('pending','failed')`, in.EffectiveReviewID); err != nil {
				return commands.Result{}, err
			}
		}
		if out.PublicationPending {
			a, err := assetControl(ctx, tx, v.AssetID)
			if err != nil {
				return commands.Result{}, err
			}
			request, err := s.ids.New()
			if err != nil {
				return commands.Result{}, err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO ledger_publication_requests(request_id,review_id,asset_id,version_id,expected_revision,status,record) VALUES(?,?,?,?,?,'pending',?)`, request, id, v.AssetID, v.VersionID, a.PublicationRevision, encoded(out)); err != nil {
				return commands.Result{}, err
			}
		}
		return commands.Result{Status: commands.ReceiptSucceeded, ResponseCode: 201, Summary: out, Events: events}, nil
	})
	if err != nil {
		return commands.Receipt{}, err
	}
	return *result.Receipt, nil
}
func (r *Reviews) satisfied(ctx context.Context, who authz.Context, v commit.Committed, t ReviewTarget, waivers []ReviewWaiver, requireQA bool, reviewer ids.ID) error {
	fail := func(message string) error { return errcode.New(errcode.ChecksNotSatisfied, message) }
	profile, err := r.profileForTarget(ctx, who, t)
	if err != nil {
		return err
	}
	// An ordinary profile upgrade is a different immutable ref. Changes or
	// revocation to this exact approved profile invalidate new acceptance.
	one, _ := canonjson.CanonicalizeValue(profile)
	two, _ := canonjson.CanonicalizeValue(t.Profile)
	if string(one) != string(two) {
		return errcode.New(errcode.ReviewTargetStale, "profile snapshot changed")
	}
	if err = r.sources.Use(ctx, who, v, profile.Profile.Purpose); err != nil {
		return err
	}
	evidence, err := r.evidence(ctx, who, v, t.Flow, t.EvidenceIDs)
	if err != nil {
		return err
	}
	one, _ = canonjson.CanonicalizeValue(evidence)
	two, _ = canonjson.CanonicalizeValue(t.Evidence)
	if string(one) != string(two) {
		return errcode.New(errcode.ReviewTargetStale, "evidence snapshot changed")
	}
	evidenceIDs := map[ids.ID]bool{}
	kinds := map[string]bool{}
	checks := map[string]AcceptedEvidence{}
	qa := false
	for _, e := range evidence {
		evidenceIDs[e.ID] = true
		kinds[e.Kind] = true
		if e.Kind == "qa_report" {
			if e.ActorID == t.MakerID {
				return errcode.New(errcode.SelfReviewForbidden, "QA must have a different principal")
			}
			if profile.Profile.DistinctActorRule == "maker_checker_reviewer" && e.ActorID == reviewer {
				return errcode.New(errcode.SelfReviewForbidden, "QA and reviewer must differ")
			}
			qa = e.QAVerdict == "pass" || qa
		}
		if e.Kind == "check_result" {
			if _, ok := checks[e.CheckKey]; ok {
				return fail("ambiguous duplicate check result")
			}
			checks[e.CheckKey] = e
		}
	}
	if (requireQA || profile.Profile.QARequired) && !qa {
		return fail("independent QA report required")
	}
	for _, kind := range profile.Profile.RequiredEvidence {
		if !kinds[kind] {
			return fail("required evidence missing")
		}
	}
	waiverByKey := map[string]ReviewWaiver{}
	for _, w := range waivers {
		if _, ok := waiverByKey[w.CheckKey]; ok || !slices.Contains(profile.Profile.WaivableChecks, w.CheckKey) || nonwaivable(w.CheckKey) || strings.TrimSpace(w.Reason) == "" || len(w.Reason) > 4096 || len(w.EvidenceIDs) == 0 {
			return fail("invalid waiver")
		}
		for _, id := range w.EvidenceIDs {
			if !evidenceIDs[id] {
				return fail("waiver evidence is not in target")
			}
		}
		if w.ExpiresAt != "" {
			at, err := clock.Parse(w.ExpiresAt)
			if err != nil || !at.After(r.ledger.clock.Now()) {
				return fail("waiver expired")
			}
		}
		waiverByKey[w.CheckKey] = w
	}
	for _, req := range profile.Profile.RequiredChecks {
		e, ok := checks[req.Key]
		if !ok {
			return fail("required check is unknown")
		}
		if e.Check == nil || e.SchemaVersion != req.SchemaVersion || e.ConfigDigest != req.ConfigDigest || !slices.Contains(req.AcceptedProcessors, e.Check.Producer) {
			return fail("processor package/version or configuration differs")
		}
		if e.Check.Verdict != "pass" {
			if _, waived := waiverByKey[req.Key]; !waived || !req.Waivable || nonwaivable(req.Key) {
				return fail("required check did not pass")
			}
		}
	}
	return nil
}
