package integration

import (
	"bytes"
	"encoding/base32"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/totp/totptest"
	"github.com/oujinhaoai/lantai/internal/ledger"
	"github.com/oujinhaoai/lantai/internal/tasks"
)

// The actor is a separately enrolled real human, not an Agent claiming a name.
func nativeOperator(t *testing.T, f *appFlow) authz.Context {
	r := f.sudo(&identity.RegisterPrincipal{Kind: authz.Human, Name: "native-operator"})
	var p struct {
		ID ids.ID `json:"principal_id"`
	}
	if json.Unmarshal(r.Summary, &p) != nil {
		t.Fatal("human registration")
	}
	setup, e := f.id.StartSetup(t.Context(), identity.SetupRequest{Name: "native-operator", Code: r.Secret, Channel: identity.ChannelCLI})
	if e != nil {
		t.Fatal(e)
	}
	if e = f.id.SetPassword(t.Context(), setup.Context, adminPassword); e != nil {
		t.Fatal(e)
	}
	enroll, e := f.id.EnrollFactor(t.Context(), setup.Context)
	if e != nil {
		t.Fatal(e)
	}
	secret, e := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(enroll.Secret)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.id.ConfirmFactor(t.Context(), setup.Context, totptest.Fresh(t, f.clk, secret)); e != nil {
		t.Fatal(e)
	}
	f.setRole(p.ID, identity.RoleContributor, true)
	s, e := f.id.Login(t.Context(), identity.LoginRequest{Name: "native-operator", Password: adminPassword, Code: totptest.Fresh(t, f.clk, secret), Channel: identity.ChannelCLI, Scopes: []identity.Scope{identity.ScopeRead, identity.ScopeIngest, identity.ScopeOrganize, identity.ScopeTask}})
	if e != nil {
		t.Fatal(e)
	}
	return s.Context
}

func nativeReportCandidate(t *testing.T, f *appFlow, payload any) (catalog.VersionResult, ledger.ReviewFlow) {
	t.Helper()
	raw, e := json.Marshal(payload)
	if e != nil {
		t.Fatal(e)
	}
	c, e := f.tasks.Create(t.Context(), f.login().Context, f.key(), tasks.CreateRequest{ProjectID: f.project.ProjectID, Type: "produce", Title: "Human-attested native protocol report", AcceptanceCriteria: []string{"synthetic protocol only; no DCC run"}, Role: identity.RoleContributor, ExpectedOutputs: []tc.OutputRequirement{{Slug: "out", AssetType: "config", CandidateCount: 1}}})
	if e != nil {
		t.Fatal(e)
	}
	attempt := f.claim(f.maker, c.TaskID)
	upload, files := f.uploadFiles(f.maker, map[string][]byte{"native-continuation.json": raw})
	files[0].Role = "record"
	rights := *rightsOwned()
	rights.NoAI = true
	v, e := f.catalog.CommitVersion(t.Context(), catalog.VersionRequest{Who: f.maker, IdempotencyKey: f.key(), UploadID: upload, Slug: "native/report-" + f.key(), Task: bindTo(c.TaskID, attempt), Content: catalog.ContentInput{AssetType: manifest.TypeConfig, Rights: &rights, Files: files}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.tasks.Submit(t.Context(), f.maker, f.key(), tasks.SubmitRequest{AttemptRef: attemptRef(f.flowEnv, c.TaskID, attempt), OutputRefs: []ids.PermanentRef{v.Ref}}); e != nil {
		t.Fatal(e)
	}
	return v, ledger.ReviewFlow{TaskID: c.TaskID, AttemptID: attempt.ID, Round: 1, Fence: attempt.Fence.LeaseFence}
}

func TestManagementNativeContinuationRealReviewHTTPS(t *testing.T) {
	f := newAppFlow(t)
	ctx := t.Context()
	producer, e := f.app.Extensions.Producer(ctx, "org.lantai.corecheck.manifest")
	if e != nil {
		t.Fatal(e)
	}
	profile := f.approvedProfile("profiles/native", profileDocument("native-continuation", producer), ids.PermanentRef{})
	f.maker = nativeOperator(t, f)
	first, _ := f.candidate("native/model", manifest.TypeModel, map[string][]byte{"model.obj": []byte("# synthetic protocol input; no DCC run\nv 0 0 0\n")}, nil)
	started := f.clk.Now()
	next, _ := f.candidateVersion("native/model-return", manifest.TypeModel, map[string][]byte{"model.obj": []byte("# synthetic protocol return; no DCC run\nv 1 0 0\n")}, nil, &first)
	finished := f.clk.Now()
	support, _ := f.candidate("native/protocol-observations", manifest.TypeDoc, map[string][]byte{"observations.txt": []byte("Synthetic protocol fixture. No native software was launched. Tests verify binding and independent human review only.")}, nil)
	ref := func(v catalog.VersionResult) map[string]any {
		return map[string]any{"asset_id": v.AssetID, "version_id": v.VersionID}
	}
	ws := saveBatchTestRecord(t, f.env, f.maker, "workset", map[string]any{"schema": "lantai.managed-workset/v1", "input": map[string]any{"schema": "lantai.client-workset/v1", "entries": []any{map[string]any{"asset_id": first.AssetID, "version_id": first.VersionID, "destination": "scene"}}, "entrypoints": []string{"scene/model.obj"}}}, nil)
	epoch, _ := f.inst.RecoveryEpoch(ctx)
	payload := map[string]any{"schema": "lantai.native-continuation/v1", "instance_id": f.inst.InstanceID(), "project_id": f.project.ProjectID, "batch_id": "native-protocol", "recovery_epoch": epoch, "workset_ref": ref(ws), "intended_purpose": "archive_review", "evidence_basis": "human_attested", "operator_id": f.maker.PrincipalID, "application": map[string]string{"name": "SyntheticProtocolFixture", "version": "1", "platform": "linux/amd64; no DCC process executed"}, "started_at": started, "finished_at": finished, "entrypoints": []string{"scene/model.obj"}, "returns": []any{map[string]any{"base": ref(first), "result": ref(next), "manifest_digest": next.ManifestDigest}}, "supporting_evidence": []any{map[string]any{"asset_id": support.AssetID, "version_id": support.VersionID, "key": "protocol-notes", "manifest_digest": support.ManifestDigest, "status": "committed"}}, "outcome": "completed", "unresolved": []string{}}
	batch := map[string]any{"schema": "lantai.migration-batch/v1", "batch_id": "native-protocol", "revision": 1, "instance_id": f.inst.InstanceID(), "project_id": f.project.ProjectID, "owner_id": f.owner.PrincipalID, "last_action": map[string]any{"actor_id": f.owner.PrincipalID, "at": f.clk.Now(), "action": "request_acceptance"}, "stage": "acceptance", "acceptance": "requested", "source_retained": true, "reference_scope": "none", "intended_purpose": "archive_review", "import_summary": map[string]int{"planned": 1, "committed": 1, "description_pending": 0, "failed": 0, "unknown": 0, "not_attempted": 0}, "evidence": map[string]any{"assets": []any{map[string]any{"asset_id": first.AssetID, "version_id": first.VersionID, "key": "model", "manifest_digest": first.ManifestDigest, "status": "committed"}}, "worksets": []any{ref(ws)}, "verifications": []any{}, "recovery": []any{}, "reference_jobs": []any{}}, "checklist": map[string]bool{}}
	// The actual protected HTTPS listener, with its explicit CA and hostname check.
	front, _, _ := recordsHTTPS(t, f.app)
	client := front.Client()
	if tr, ok := client.Transport.(*http.Transport); !ok || tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("strict TLS required")
	}
	var lastRecord catalog.VersionResult
	var readOnlyCheck bool
	check := func(native []any) (string, string) {
		t.Helper()
		batch["last_action"].(map[string]any)["at"] = f.clk.Now()
		batch["evidence"].(map[string]any)["native_continuations"] = native
		if !readOnlyCheck {
			lastRecord = saveBatchTestRecord(t, f.env, f.login().Context, "migration", batch, nil)
		}
		v := lastRecord
		body, _ := json.Marshal(ref(v))
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+"/api/v1/management/batches/check", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+f.login().Token)
		req.Header.Set("Content-Type", "application/json")
		response, e := client.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		if response.StatusCode != 200 {
			t.Fatalf("batch status %d: %s", response.StatusCode, raw)
		}
		var out struct {
			Acceptance string `json:"acceptance"`
			Checks     []struct{ Key, Status, Code string }
		}
		if json.Unmarshal(raw, &out) != nil {
			t.Fatal("check JSON")
		}
		for _, c := range out.Checks {
			if c.Key == "native_continuation" {
				return c.Status, c.Code
			}
		}
		t.Fatal("native check omitted")
		return "", ""
	}
	require := func(want string, refs ...any) {
		t.Helper()
		status, code := check(refs)
		if status != want {
			t.Fatalf("native status=%s code=%s want=%s", status, code, want)
		}
		t.Logf("native=%s code=%s", status, code)
	}
	require("unknown")
	report, flow := nativeReportCandidate(t, f, payload)
	require("unknown", ref(report))
	job := f.checkCandidate(report, flow, "org.lantai.corecheck.manifest")
	target := f.submitChecked(report, flow, profile.Ref, job, false)
	decision := f.reviewDecision(target, nil)
	decision.Reason = "Independent review of synthetic native-continuation protocol binding only; no DCC execution claimed"
	review, e := f.recordDecision(decision)
	if e != nil {
		t.Fatal(e)
	}
	require("passed", ref(report))
	// Existing policy can require independent review; this genuine two-human
	// approval remains applicable without inventing a second approval system.
	f.sudo(&identity.SetPolicy{ProjectID: f.project.ProjectID, Key: "review.allow_self_human", Value: json.RawMessage(`false`)})
	require("passed", ref(report))
	f.sudo(&identity.SetPolicy{ProjectID: f.project.ProjectID, Key: "review.allow_self_human", Value: json.RawMessage(`true`), ExpectedRevision: 1})
	require("failed", ref(report), ref(report))
	// Authenticated, genuinely reviewed but wrong-bound reports still fail.
	for _, bad := range []string{"epoch", "batch", "operator", "entrypoint", "return_hash", "unchanged_return", "unresolved", "machine_claim", "unknown_field", "renamed_return", "deleted_entrypoint"} {
		t.Run(bad, func(t *testing.T) {
			raw, _ := json.Marshal(payload)
			var p map[string]any
			_ = json.Unmarshal(raw, &p)
			originalAssets := batch["evidence"].(map[string]any)["assets"]
			originalWorksets := batch["evidence"].(map[string]any)["worksets"]
			defer func() {
				batch["evidence"].(map[string]any)["assets"] = originalAssets
				batch["evidence"].(map[string]any)["worksets"] = originalWorksets
			}()
			switch bad {
			case "renamed_return", "deleted_entrypoint":
				content := []byte("synthetic unchanged project bytes")
				base, _ := f.candidate("native/"+bad, manifest.TypeModel, map[string][]byte{"model.obj": content}, nil)
				output := map[string][]byte{"renamed.obj": content}
				if bad == "deleted_entrypoint" {
					output = map[string][]byte{"notes.txt": []byte("new bytes but original project entrypoint deleted")}
				}
				result, _ := f.candidateVersion("native/"+bad+"-return", manifest.TypeModel, output, nil, &base)
				badWS := saveBatchTestRecord(t, f.env, f.maker, "workset", map[string]any{"schema": "lantai.managed-workset/v1", "input": map[string]any{"schema": "lantai.client-workset/v1", "entries": []any{map[string]any{"asset_id": base.AssetID, "version_id": base.VersionID, "destination": "scene"}}, "entrypoints": []string{"scene/model.obj"}}}, nil)
				p["workset_ref"] = ref(badWS)
				p["started_at"] = f.clk.Now()
				p["finished_at"] = f.clk.Now()
				p["returns"] = []any{map[string]any{"base": ref(base), "result": ref(result), "manifest_digest": result.ManifestDigest}}
				batch["evidence"].(map[string]any)["assets"] = []any{map[string]any{"asset_id": base.AssetID, "version_id": base.VersionID, "key": "model", "manifest_digest": base.ManifestDigest, "status": "committed"}}
				batch["evidence"].(map[string]any)["worksets"] = []any{ref(badWS)}
			case "epoch":
				p["recovery_epoch"] = epoch + 1
			case "batch":
				p["batch_id"] = "foreign"
			case "operator":
				p["operator_id"] = f.owner.PrincipalID
			case "entrypoint":
				p["entrypoints"] = []string{"scene/missing.obj"}
			case "return_hash":
				p["returns"].([]any)[0].(map[string]any)["manifest_digest"] = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
			case "unchanged_return":
				p["returns"].([]any)[0].(map[string]any)["result"] = ref(first)
			case "unresolved":
				p["unresolved"] = []string{"native result uncertain"}
			case "machine_claim":
				p["evidence_basis"] = "machine_verified"
			case "unknown_field":
				p["dcc_verified"] = true
			}
			v, flow := nativeReportCandidate(t, f, p)
			j := f.checkCandidate(v, flow, "org.lantai.corecheck.manifest")
			target := f.submitChecked(v, flow, profile.Ref, j, false)
			if _, e := f.recordDecision(f.reviewDecision(target, nil)); e != nil {
				t.Fatal(e)
			}
			want := "failed"
			if bad == "unresolved" {
				want = "unknown"
			}
			require(want, ref(v))
		})
	}
	// Current human membership/reviewer permissions are re-evaluated.
	readOnlyCheck = true
	f.setRole(f.owner.PrincipalID, identity.RoleViewer, true)
	f.setRole(f.owner.PrincipalID, identity.RoleOwner, false)
	require("failed", ref(report))
	f.setRole(f.owner.PrincipalID, identity.RoleOwner, true)
	readOnlyCheck = false
	require("passed", ref(report))
	f.setRole(f.maker.PrincipalID, identity.RoleContributor, false)
	require("failed", ref(report))
	f.setRole(f.maker.PrincipalID, identity.RoleContributor, true)
	require("passed", ref(report))
	profileState, e := f.ledger.VersionControl(ctx, profile.VersionID)
	if e != nil {
		t.Fatal(e)
	}
	profileFact, e := f.reviews.ReviewByID(ctx, profileState.EffectiveReviewID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.recordDecision(ledger.ReviewDecision{Action: identity.ActRevokeReview, TargetID: profileFact.Target.ID, ExpectedRevision: profileState.Revision, EffectiveReviewID: profileFact.Review.ID, Verdict: "revoke", Reason: "Synthetic native profile revocation", Waivers: []ledger.ReviewWaiver{}}); e != nil {
		t.Fatal(e)
	}
	require("failed", ref(report))
	// Revoke the exact review through a new real TOTP grant, not a SQL update.
	state, e := f.ledger.VersionControl(ctx, report.VersionID)
	if e != nil {
		t.Fatal(e)
	}
	revoke := ledger.ReviewDecision{Action: identity.ActRevokeReview, TargetID: target.ID, ExpectedRevision: state.Revision, EffectiveReviewID: review.ID, Verdict: "revoke", Reason: "Synthetic review revocation regression", Waivers: []ledger.ReviewWaiver{}}
	if _, e := f.recordDecision(revoke); e != nil {
		t.Fatal(e)
	}
	require("unknown", ref(report))
	t.Logf("real core/T05/corecheck/T03/TOTP/HumanGrant and strict HTTPS native protocol passed; profile=%s; no DCC or browser run", profile.VersionID)
}
