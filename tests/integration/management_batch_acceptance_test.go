package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

func saveBatchTestRecord(t *testing.T, e *env, who authz.Context, kind string, payload any, previous *catalog.VersionResult) catalog.VersionResult {
	t.Helper()
	b, err := json.Marshal(map[string]any{"schema": "lantai.management-record/v1", "kind": kind, "instance_id": e.inst.InstanceID(), "project_id": e.project.ProjectID, "payload": payload})
	if err != nil {
		t.Fatal(err)
	}
	rights := manifest.Rights{Usage: "reference", License: "LicenseRef-Lantai-Private-Record", RedistributeRaw: false, NoAI: true, Sensitivity: "normal"}
	in := catalog.VersionRequest{Who: who, IdempotencyKey: e.key(), UploadID: e.upload(who, b), Slug: fmt.Sprintf("test-records/%s-%s", kind, e.key()), Content: catalog.ContentInput{AssetType: manifest.TypeConfig, Rights: &rights, Files: []manifest.InputFile{{Path: "record.json", Role: "record", SHA256: shaOf(b), Size: int64(len(b))}}}}
	if previous != nil {
		in.Slug = ""
		in.AssetID = previous.AssetID
		in.BaseVersionID = previous.VersionID
	}
	v, err := e.catalog.CommitVersion(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestManagementBatchReferencesRequireOriginalCoreJobs(t *testing.T) {
	f := newDeviceWorkerCoreFixture(t)
	f.runEightActions()
	var planJob struct {
		Input struct {
			Mappings []struct {
				FixedRef string `json:"fixed_ref"`
			} `json:"mappings"`
		} `json:"input"`
	}
	if json.Unmarshal(f.jobs["references.plan"].Job, &planJob) != nil || len(planJob.Input.Mappings) != 1 {
		t.Fatal("missing actual fixed reference plan")
	}
	fixed, err := ids.ParseURI(planJob.Input.Mappings[0].FixedRef)
	if err != nil {
		t.Fatal(err)
	}
	v, err := f.app.Catalog.GetVersion(t.Context(), f.browser.Context, fixed.AssetID, fixed.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	jobs := []any{}
	for _, action := range []string{"references.plan", "references.copy", "references.rollback"} {
		jobs = append(jobs, map[string]any{"job_id": f.jobs[action].ID, "action": action})
	}
	batch := map[string]any{
		"schema": "lantai.migration-batch/v1", "batch_id": "synthetic-device-batch", "revision": 1, "instance_id": f.env.inst.InstanceID(), "project_id": f.env.project.ProjectID, "owner_id": f.browser.Context.PrincipalID,
		"last_action": map[string]any{"actor_id": f.browser.Context.PrincipalID, "at": f.env.clk.Now(), "action": "verify_reference_rollback"}, "stage": "rollback", "acceptance": "requested", "source_retained": true, "reference_scope": "selected_notes", "intended_purpose": "archive_review",
		"import_summary": map[string]int{"planned": 1, "committed": 1},
		"evidence":       map[string]any{"assets": []any{map[string]any{"key": "sample", "asset_id": fixed.AssetID, "version_id": fixed.VersionID, "manifest_digest": v.Version.ManifestDigest, "status": "committed"}}, "worksets": []any{}, "verifications": []any{}, "recovery": []any{}, "reference_jobs": jobs},
		"checklist":      map[string]bool{},
	}
	check := func(record catalog.VersionResult) map[string]string {
		t.Helper()
		var out struct {
			Acceptance string `json:"acceptance"`
			Checks     []struct {
				Key    string `json:"key"`
				Status string `json:"status"`
				Code   string `json:"code"`
			} `json:"checks"`
		}
		f.web(http.MethodPost, "/api/v1/management/batches/check", map[string]any{"asset_id": record.AssetID, "version_id": record.VersionID}, &out)
		if out.Acceptance != "blocked" {
			t.Fatal("reference job history replaced missing restore evidence")
		}
		statuses := map[string]string{}
		for _, row := range out.Checks {
			statuses[row.Key] = row.Status
			if row.Key == "references" && row.Status != "passed" {
				t.Logf("references: %s %s", row.Status, row.Code)
			}
		}
		return statuses
	}
	record := saveBatchTestRecord(t, f.env, f.browser.Context, "migration", batch, nil)
	if statuses := check(record); statuses["references"] != "passed" || statuses["record"] != "passed" || statuses["assets"] != "passed" {
		t.Fatalf("actual eight-action core history was not accepted for selected notes: %v", statuses)
	}
	// A different successful action cannot masquerade as a reference copy.
	jobs[1] = map[string]any{"job_id": f.jobs["workset.pull"].ID, "action": "references.copy"}
	batch["revision"] = 2
	batch["previous_ref"] = map[string]any{"asset_id": record.AssetID, "version_id": record.VersionID}
	bad := saveBatchTestRecord(t, f.env, f.browser.Context, "migration", batch, &record)
	if statuses := check(bad); statuses["references"] != "failed" {
		t.Fatalf("unrelated successful job accepted as copy: %v", statuses)
	}
	jobs[1] = map[string]any{"job_id": f.jobs["references.copy"].ID, "action": "references.copy"}
	batch["evidence"].(map[string]any)["reference_jobs"] = jobs[:2]
	batch["revision"] = 3
	batch["previous_ref"] = map[string]any{"asset_id": bad.AssetID, "version_id": bad.VersionID}
	missing := saveBatchTestRecord(t, f.env, f.browser.Context, "migration", batch, &bad)
	if statuses := check(missing); statuses["references"] != "missing" {
		t.Fatalf("rollback stage accepted without rollback: %v", statuses)
	}
}

// The server recomputes the current fixed content. Client-created receipts and
// complete/accepted fields cannot override missing restore/native evidence.
func TestManagementBatchAcceptanceCurrentEvidenceAndRevocation(t *testing.T) {
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	e.setRole(e.admin.Context.PrincipalID, identity.RoleOwner, true)
	_, outsider := e.agent("batch-outsider@synthetic", "")
	a := reopenApplication(t, e)
	who := e.login()
	asset := e.ingest(who.Context, "batch/synthetic", []byte("synthetic fixed content"), *rightsOwned())
	version, err := a.Catalog.GetVersion(t.Context(), who.Context, asset.AssetID, asset.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	ref := func(v catalog.VersionResult) map[string]any {
		return map[string]any{"asset_id": v.AssetID, "version_id": v.VersionID}
	}
	worksetPayload := map[string]any{"schema": "lantai.managed-workset/v1", "title": "Synthetic fixed workset", "input": map[string]any{"schema": "lantai.client-workset/v1", "entries": []any{map[string]any{"asset_id": asset.AssetID, "version_id": asset.VersionID, "destination": "assets/synthetic"}}}}
	workset := saveBatchTestRecord(t, e, who.Context, "workset", worksetPayload, nil)
	receipt := map[string]any{"schema": "lantai.browser-verification/v1", "instance_id": e.inst.InstanceID(), "asset_id": asset.AssetID, "version_id": asset.VersionID, "manifest_digest": asset.ManifestDigest, "verified_at": e.clk.Now(), "files": version.Manifest.Content.Files, "current_remote_verified": true, "completion_basis": "current_remote_and_downloaded_files"}
	verificationPayload := map[string]any{"schema": "lantai.migration-verification-record/v1", "batch_id": "synthetic-batch", "instance_id": e.inst.InstanceID(), "project_id": e.project.ProjectID, "verifications": []any{receipt}}
	verification := saveBatchTestRecord(t, e, who.Context, "migration", verificationPayload, nil)
	batch := map[string]any{
		"schema": "lantai.migration-batch/v1", "batch_id": "synthetic-batch", "revision": 1, "instance_id": e.inst.InstanceID(), "project_id": e.project.ProjectID, "owner_id": who.Context.PrincipalID,
		"last_action": map[string]any{"actor_id": who.Context.PrincipalID, "at": e.clk.Now(), "action": "request_acceptance"}, "stage": "acceptance", "acceptance": "requested", "source_retained": true, "reference_scope": "none", "intended_purpose": "archive_review",
		"import_summary": map[string]any{"planned": 1, "committed": 1, "description_pending": 0, "failed": 0, "unknown": 0, "not_attempted": 0},
		"evidence":       map[string]any{"assets": []any{map[string]any{"key": "synthetic", "asset_id": asset.AssetID, "version_id": asset.VersionID, "manifest_digest": asset.ManifestDigest, "status": "committed"}}, "worksets": []any{ref(workset)}, "verifications": []any{ref(verification)}, "recovery": []any{}, "reference_jobs": []any{}},
		"checklist":      map[string]bool{"excluded_backed_up": true, "working_inputs_backed_up": true, "entrypoints_opened": true, "new_results_captured": true, "fallback_ready": true, "selected_notes_reviewed": true},
		"complete":       true, "accepted": true,
	}
	first := saveBatchTestRecord(t, e, who.Context, "migration", batch, nil)
	if current, cause := a.Catalog.GetAsset(t.Context(), who.Context, first.AssetID); cause != nil {
		t.Fatalf("current management record unavailable: %v", cause)
	} else if current.Latest.VersionID != first.VersionID {
		t.Fatal("current management record version changed")
	}
	if fixed, cause := a.Catalog.GetVersion(t.Context(), who.Context, first.AssetID, first.VersionID); cause != nil || fixed.Version.CommittedBy != who.Context.PrincipalID {
		t.Fatalf("management actor binding: %s %v", fixed.Version.CommittedBy, cause)
	}
	check := func(v catalog.VersionResult) (string, map[string]string) {
		t.Helper()
		b, err := a.CheckManagementBatch(t.Context(), who.Context, v.AssetID, v.VersionID)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Acceptance string `json:"acceptance"`
			Checks     []struct {
				Key    string `json:"key"`
				Status string `json:"status"`
			} `json:"checks"`
		}
		if err = json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		checks := map[string]string{}
		for _, c := range got.Checks {
			checks[c.Key] = c.Status
		}
		return got.Acceptance, checks
	}
	status, checks := check(first)
	if status != "blocked" || checks["record"] != "passed" || checks["assets"] != "passed" || checks["worksets"] != "passed" || checks["verifications"] != "passed" || checks["recovery"] != "failed" {
		t.Fatalf("client acceptance claim replaced current evidence: %s %v", status, checks)
	}

	// A declared native entrypoint is a separate gate even when all files hash.
	worksetPayload["input"].(map[string]any)["entrypoints"] = []string{"assets/synthetic/content.txt"}
	nativeWorkset := saveBatchTestRecord(t, e, who.Context, "workset", worksetPayload, nil)
	batch["evidence"].(map[string]any)["worksets"] = []any{ref(nativeWorkset)}
	batch["revision"] = 2
	batch["previous_ref"] = ref(first)
	second := saveBatchTestRecord(t, e, who.Context, "migration", batch, &first)
	status, checks = check(second)
	if status != "blocked" || checks["native_continuation"] != "unknown" || checks["record"] != "passed" {
		t.Fatalf("unverified native continuation accepted: %s %v", status, checks)
	}
	_, checks = check(first)
	if checks["record"] != "failed" {
		t.Fatal("historical batch revision accepted as current")
	}

	// A browser record containing the wrong fixed hash never supplies evidence.
	receipt["manifest_digest"] = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	badVerification := saveBatchTestRecord(t, e, who.Context, "migration", verificationPayload, nil)
	batch["evidence"].(map[string]any)["verifications"] = []any{ref(badVerification)}
	batch["import_summary"].(map[string]any)["unknown"] = 1
	batch["revision"] = 3
	batch["previous_ref"] = ref(second)
	third := saveBatchTestRecord(t, e, who.Context, "migration", batch, &second)
	_, checks = check(third)
	if checks["assets"] != "failed" || checks["verifications"] != "failed" {
		t.Fatalf("unknown import or forged receipt passed: %v", checks)
	}

	servers := assetRemoteServers(t, a)
	post := func(token string) int {
		t.Helper()
		body, _ := json.Marshal(ref(third))
		r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+servers.Addresses.API+"/api/v1/management/batches/check", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		_, _ = io.Copy(io.Discard, res.Body)
		return res.StatusCode
	}
	if got := post(who.Token); got != 200 {
		t.Fatalf("authorized fixed check: %d", got)
	}
	if got := post(outsider.Token); got != 404 {
		t.Fatalf("unrelated principal learned fixed batch: %d", got)
	}
	e.setRole(who.Context.PrincipalID, identity.RoleOwner, false)
	if got := post(who.Token); got != 404 {
		t.Fatalf("membership revocation did not filter record: %d", got)
	}
	if err := a.Identity.EndSession(t.Context(), who.Context, who.Context.SessionID); err != nil {
		t.Fatal(err)
	}
	if got := post(who.Token); got != 401 {
		t.Fatalf("session revocation did not deny batch: %d", got)
	}
}
