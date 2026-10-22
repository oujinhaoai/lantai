package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/ledger"
)

// One actual enrolled human supplies both production and review. Agents and the
// corecheck worker retain their real machine identities; no approval uses SQL.
func TestManagementNativeSingleHumanPolicyAndFreshFactorsHTTPS(t *testing.T) {
	ctx := t.Context()
	f := newAppFlow(t)
	machine := f.maker
	producer, e := f.app.Extensions.Producer(ctx, "org.lantai.corecheck.manifest")
	if e != nil {
		t.Fatal(e)
	}
	profile := f.approvedProfile("profiles/native-self", profileDocument("native-continuation", producer), ids.PermanentRef{})
	f.setRole(f.owner.PrincipalID, identity.RoleContributor, true)
	f.maker = f.owner
	members, e := f.id.ProjectRecipients(ctx, f.project.ProjectID)
	if e != nil {
		t.Fatal(e)
	}
	humans := 0
	for _, p := range members {
		if p.Kind == authz.Human {
			humans++
		}
	}
	if humans != 1 {
		t.Fatalf("single-human fixture has %d humans", humans)
	}
	first, _ := f.candidate("native-self/model", manifest.TypeModel, map[string][]byte{"model.obj": []byte("# synthetic only\nv 0 0 0\n")}, nil)
	started := f.clk.Now()
	next, _ := f.candidateVersion("native-self/model-return", manifest.TypeModel, map[string][]byte{"model.obj": []byte("# synthetic only\nv 1 0 0\n")}, nil, &first)
	finished := f.clk.Now()
	support, _ := f.candidate("native-self/observations", manifest.TypeDoc, map[string][]byte{"notes.txt": []byte("Synthetic human-attestation protocol; no DCC process executed.")}, nil)
	ref := func(v catalog.VersionResult) map[string]any {
		return map[string]any{"asset_id": v.AssetID, "version_id": v.VersionID}
	}
	workset := saveBatchTestRecord(t, f.env, f.maker, "workset", map[string]any{"schema": "lantai.managed-workset/v1", "input": map[string]any{"schema": "lantai.client-workset/v1", "entries": []any{map[string]any{"asset_id": first.AssetID, "version_id": first.VersionID, "destination": "scene"}}, "entrypoints": []string{"scene/model.obj"}}}, nil)
	epoch, _ := f.inst.RecoveryEpoch(ctx)
	payload := map[string]any{"schema": "lantai.native-continuation/v1", "instance_id": f.inst.InstanceID(), "project_id": f.project.ProjectID, "batch_id": "native-single-human", "recovery_epoch": epoch, "workset_ref": ref(workset), "intended_purpose": "archive_review", "evidence_basis": "human_attested", "operator_id": f.owner.PrincipalID, "application": map[string]string{"name": "SyntheticPolicyFixture", "version": "1", "platform": "linux/amd64; no DCC process executed"}, "started_at": started, "finished_at": finished, "entrypoints": []string{"scene/model.obj"}, "returns": []any{map[string]any{"base": ref(first), "result": ref(next), "manifest_digest": next.ManifestDigest}}, "supporting_evidence": []any{map[string]any{"asset_id": support.AssetID, "version_id": support.VersionID, "key": "protocol-notes", "manifest_digest": support.ManifestDigest, "status": "committed"}}, "outcome": "completed", "unresolved": []string{}}
	report, flow := nativeReportCandidate(t, f, payload)
	job := f.checkCandidate(report, flow, "org.lantai.corecheck.manifest")
	target := f.submitChecked(report, flow, profile.Ref, job, false)
	decision := f.reviewDecision(target, nil)
	action, e := f.reviews.HumanAction(ctx, decision)
	if e != nil {
		t.Fatal(e)
	}
	batch := map[string]any{"schema": "lantai.migration-batch/v1", "batch_id": "native-single-human", "revision": 1, "instance_id": f.inst.InstanceID(), "project_id": f.project.ProjectID, "owner_id": f.owner.PrincipalID, "last_action": map[string]any{"actor_id": f.owner.PrincipalID, "at": f.clk.Now(), "action": "request_acceptance"}, "stage": "acceptance", "acceptance": "requested", "source_retained": true, "reference_scope": "none", "intended_purpose": "archive_review", "import_summary": map[string]int{"planned": 1, "committed": 1}, "evidence": map[string]any{"assets": []any{map[string]any{"asset_id": first.AssetID, "version_id": first.VersionID, "key": "model", "manifest_digest": first.ManifestDigest, "status": "committed"}}, "worksets": []any{ref(workset)}, "native_continuations": []any{ref(report)}, "verifications": []any{}, "recovery": []any{}, "reference_jobs": []any{}}, "checklist": map[string]bool{}}
	record := saveBatchTestRecord(t, f.env, f.login().Context, "migration", batch, nil)
	front, _, _ := recordsHTTPS(t, f.app)
	client := front.Client()
	if tr, ok := client.Transport.(*http.Transport); !ok || tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("strict TLS required")
	}
	check := func(want string) {
		t.Helper()
		raw, _ := json.Marshal(ref(record))
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+"/api/v1/management/batches/check", bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+f.login().Token)
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		if response.StatusCode != 200 {
			t.Fatalf("batch status %d: %s", response.StatusCode, body)
		}
		var out struct {
			Checks []struct{ Key, Status, Code string }
		}
		if json.Unmarshal(body, &out) != nil {
			t.Fatal("batch JSON")
		}
		for _, c := range out.Checks {
			if c.Key == "native_continuation" {
				if c.Status != want {
					t.Fatalf("native %s/%s want %s", c.Status, c.Code, want)
				}
				t.Logf("native=%s code=%s", c.Status, c.Code)
				return
			}
		}
		t.Fatal("native result missing")
	}
	pending := func() {
		t.Helper()
		s, err := f.ledger.VersionControl(ctx, report.VersionID)
		if err != nil || s.ReviewState == "approved" || s.EffectiveReviewID.Valid() {
			t.Fatalf("denied approval changed ledger: %+v %v", s, err)
		}
		check("unknown")
	}
	// A real Agent and Worker cannot request or consume human authorization.
	for _, actor := range []authz.Context{machine, f.worker} {
		if _, err := f.id.CreateDomainChallenge(ctx, actor, []identity.HumanAction{action}, f.app); errcode.CodeOf(err) != errcode.Forbidden {
			t.Fatalf("machine human challenge: %v", err)
		}
		if _, err := f.reviews.Record(ctx, actor, decision, "", ids.New(), f.id); errcode.CodeOf(err) != errcode.Forbidden {
			t.Fatalf("machine review: %v", err)
		}
	}
	pending()
	// Even a fresh TOTP-authenticated login is not a decision-bound HumanGrant.
	who := f.login().Context
	if _, err := f.id.CreateDomainChallenge(ctx, who, []identity.HumanAction{action}, f.app); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reviews.Record(ctx, who, decision, "", ids.New(), f.id); errcode.CodeOf(err) != errcode.HumanProofRequired {
		t.Fatalf("unverified challenge accepted: %v", err)
	}
	pending()
	who, g, op := f.grant(action)
	changed := decision
	changed.Reason += " changed after approval"
	if _, err := f.reviews.Record(ctx, who, changed, g, op, f.id); errcode.CodeOf(err) != errcode.HumanGrantMismatch {
		t.Fatalf("changed decision accepted: %v", err)
	}
	pending()
	if err := f.id.RevokeGrant(ctx, who, g); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reviews.Record(ctx, who, decision, g, op, f.id); errcode.CodeOf(err) != errcode.HumanProofRequired {
		t.Fatalf("revoked grant accepted: %v", err)
	}
	pending()
	who, g, op = f.grant(action)
	f.clk.Advance(6 * time.Minute)
	if _, err := f.reviews.Record(ctx, who, decision, g, op, f.id); errcode.CodeOf(err) != errcode.HumanProofRequired {
		t.Fatalf("expired grant accepted: %v", err)
	}
	pending()
	// The normal existing TOTP flow authorizes the sole human to self-review.
	who, g, op = f.grant(action)
	receipt, e := f.reviews.Record(ctx, who, decision, g, op, f.id)
	if e != nil {
		t.Fatal(e)
	}
	var review ledger.Review
	if json.Unmarshal(receipt.ResponseSummary, &review) != nil || !review.SelfReview || review.ActorID != f.owner.PrincipalID || !review.HumanGrantID.Valid() {
		t.Fatalf("self-review audit missing: %+v", review)
	}
	fact, e := f.reviews.ReviewByID(ctx, review.ID)
	if e != nil {
		t.Fatal(e)
	}
	check("passed")
	if e = f.reviews.ValidateEffectiveApproval(ctx, f.login().Context, fact); e != nil {
		t.Fatal(e)
	}
	// Historical authorization expiry does not erase an accepted decision or add
	// a second review when the original exact operation is legitimately replayed.
	before, e := f.ledger.VersionControl(ctx, report.VersionID)
	if e != nil {
		t.Fatal(e)
	}
	f.clk.Advance(6 * time.Minute)
	replay, e := f.reviews.Record(ctx, who, decision, g, op, f.id)
	if e != nil || !bytes.Equal(replay.ResponseSummary, receipt.ResponseSummary) {
		t.Fatalf("original receipt replay: %v", e)
	}
	after, e := f.ledger.VersionControl(ctx, report.VersionID)
	if e != nil || before != after {
		t.Fatalf("replay changed approval: %+v %+v %v", before, after, e)
	}
	check("passed")
	// Current policy tightening invalidates new uses without rewriting history.
	f.sudo(&identity.SetPolicy{ProjectID: f.project.ProjectID, Key: "review.allow_self_human", Value: json.RawMessage(`false`)})
	if e = f.reviews.ValidateEffectiveApproval(ctx, f.login().Context, fact); errcode.CodeOf(e) != errcode.SelfReviewForbidden {
		t.Fatalf("tightened policy ignored: %v", e)
	}
	check("failed")
	state, e := f.ledger.VersionControl(ctx, report.VersionID)
	if e != nil || state.EffectiveReviewID != review.ID || state.ReviewState != "approved" {
		t.Fatal("policy rewrote review history", e)
	}
	f.maker = f.login().Context
	second, secondFlow := nativeReportCandidate(t, f, payload)
	secondJob := f.checkCandidate(second, secondFlow, "org.lantai.corecheck.manifest")
	secondTarget := f.submitChecked(second, secondFlow, profile.Ref, secondJob, false)
	if _, e = f.recordDecision(f.reviewDecision(secondTarget, nil)); errcode.CodeOf(e) != errcode.SelfReviewForbidden {
		t.Fatalf("policy-forbidden new self approval: %v", e)
	}
	state, e = f.ledger.VersionControl(ctx, second.VersionID)
	if e != nil || state.ReviewState == "approved" || state.EffectiveReviewID.Valid() {
		t.Fatal("forbidden self approval committed", e)
	}
	f.sudo(&identity.SetPolicy{ProjectID: f.project.ProjectID, Key: "review.allow_self_human", Value: json.RawMessage(`true`), ExpectedRevision: 1})
	check("passed")
	t.Log("one actual human; current T03 policy; real fresh TOTP/HumanGrant; machine/missing/wrong/revoked/expired proofs denied; immutable self-review and exact operation replay; no DCC machine proof")
}
