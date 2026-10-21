package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	dc "github.com/oujinhaoai/lantai/internal/contract/device"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/localassistant"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// Uses real T01 sessions, live roles, HTTP guards and the original T06 job
// tables. Real filesystem/HTTPS eight-action execution is covered separately
// by the core-to-localassistant worker integration, not by synthetic reports.
func TestDeviceControlAuthorizationFencesRestartAndRevocation(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	e.setRole(e.admin.Context.PrincipalID, identity.RoleContributor, true)
	_, agentSession := e.agent("device-control@node", identity.RoleViewer)
	a := reopenApplication(t, e)
	servers := backupHTTPServers(t, a)
	base := "http://" + servers.Addresses.API
	web := backupLogin(t, e, base, []identity.Scope{identity.ScopeRead, identity.ScopeOrganize})
	readOnly := backupLogin(t, e, base, []identity.Scope{identity.ScopeRead})
	anonymous := backupBrowser{t: t, base: base}
	agent := backupBrowser{t: t, base: base, token: agentSession.Token}
	shortSession := func() identity.IssuedSession {
		t.Helper()
		s, err := e.id.Login(ctx, identity.LoginRequest{Name: "ada", Password: adminPassword, Code: e.fresh(), Channel: identity.ChannelCLI, Scopes: []identity.Scope{identity.ScopeRead, identity.ScopeIngest, identity.ScopeOrganize}, Projects: []ids.ID{e.project.ProjectID}, TTL: 10 * time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	deviceSession := shortSession()
	worker := backupBrowser{t: t, base: base, token: deviceSession.Token}
	deny := func(browser backupBrowser, method, path, key string, body any, code errcode.Code) {
		t.Helper()
		status, raw, _ := browser.request(method, path, key, body)
		var envelope struct {
			Error struct {
				Code errcode.Code `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}
		if status < 400 || envelope.Error.Code != code {
			t.Fatalf("%s %s: status=%d code=%s want=%s body=%s", method, path, status, envelope.Error.Code, code, raw)
		}
	}
	claimInput := dc.Claim{ProjectID: e.project.ProjectID, Alias: "synthetic-control", WorkspaceAlias: "chosen", WorkspaceBinding: strings.Repeat("a", 64), Actions: []string{"import.plan", "import.apply"}}
	deny(anonymous, http.MethodGet, "devices?project_id="+string(e.project.ProjectID), "", nil, errcode.AuthRequired)
	deny(web, http.MethodPost, "devices/claims", "browser-cannot-enroll", claimInput, errcode.Forbidden)
	deny(agent, http.MethodPost, "devices/claims", "agent-cannot-enroll", claimInput, errcode.Forbidden)

	// Enrollment cannot turn a broad or privileged human login into a worker.
	for _, trial := range []struct {
		name     string
		ttl      time.Duration
		projects []ids.ID
		scopes   []identity.Scope
	}{
		{"overlong", 16 * time.Minute, []ids.ID{e.project.ProjectID}, []identity.Scope{identity.ScopeRead, identity.ScopeOrganize}},
		{"unscoped", 10 * time.Minute, nil, []identity.Scope{identity.ScopeRead, identity.ScopeOrganize}},
		{"admin-scope", 10 * time.Minute, []ids.ID{e.project.ProjectID}, []identity.Scope{identity.ScopeRead, identity.ScopeOrganize, identity.ScopeAdmin}},
	} {
		issued, err := e.id.Login(ctx, identity.LoginRequest{Name: "ada", Password: adminPassword, Code: e.fresh(), Channel: identity.ChannelCLI, Scopes: trial.scopes, Projects: trial.projects, TTL: trial.ttl})
		if err != nil {
			t.Fatal(err)
		}
		deny(backupBrowser{t: t, base: base, token: issued.Token}, http.MethodPost, "devices/claims", "denied-"+trial.name, claimInput, errcode.Forbidden)
	}
	var claim dc.ClaimResponse
	worker.must(http.MethodPost, "devices/claims", "original-claim", claimInput, &claim, 200)
	if claim.ClaimCode == "" || claim.Device.State != "pending" || claim.Device.OwnerID != deviceSession.Context.PrincipalID {
		t.Fatal("claim did not bind an independent T01 session")
	}
	var replayClaim dc.ClaimResponse
	worker.must(http.MethodPost, "devices/claims", "original-claim", claimInput, &replayClaim, 200)
	if replayClaim.Device.ID != claim.Device.ID || replayClaim.ClaimCode != "" {
		t.Fatal("claim replay created a device or persisted its one-time code")
	}
	devicePath := "devices/" + string(claim.Device.ID)
	deny(worker, http.MethodPost, devicePath+"/poll", "pending-poll", struct{}{}, errcode.Forbidden)
	approve := dc.ApproveDevice{ExpectedRevision: claim.Device.Revision, ClaimCode: claim.ClaimCode, WorkspaceAlias: claim.Device.WorkspaceAlias, WorkspaceBinding: claim.Device.WorkspaceBinding}
	wrong := approve
	wrong.ClaimCode = "wrong"
	deny(web, http.MethodPost, devicePath+"/approve", "wrong-code", wrong, errcode.Forbidden)
	deny(readOnly, http.MethodPost, devicePath+"/approve", "scope-denied", approve, errcode.Forbidden)
	var device dc.Device
	web.must(http.MethodPost, devicePath+"/approve", "approve-device", approve, &device, 200)
	if device.State != "active" {
		t.Fatal("device was not approved")
	}
	var listed dc.DevicePage
	web.must(http.MethodGet, "devices?project_id="+string(e.project.ProjectID)+"&limit=1", "", nil, &listed, 200)
	if len(listed.Items) != 1 || listed.Items[0].ID != device.ID {
		t.Fatal("authorized device discovery failed")
	}
	newJob := func(name string) (dc.CreateJob, localassistant.Job) {
		t.Helper()
		input := client.ImportInput{Schema: client.ImportInputSchema, Assets: []client.ImportAsset{{Key: "selected", Directory: ".", Classification: "final", Push: client.PushInput{ProjectID: string(e.project.ProjectID), Slug: "device-control/selected", Content: client.ContentInput{AssetType: "doc", Files: []client.InputFile{{Path: "selected.txt", Role: "primary"}}, Rights: json.RawMessage(`{"usage":"reference","license":"LicenseRef-Synthetic","redistribute_raw":false,"noai":true,"sensitivity":"normal"}`)}}}}}
		raw, _ := json.Marshal(input)
		spec := localassistant.Job{Schema: localassistant.JobSchema, JobID: name, Action: "import.plan", Input: raw, Directory: "sources", Output: "reports/" + name + ".json"}
		body, _ := json.Marshal(spec)
		canonical, err := canonjson.Canonicalize(body)
		if err != nil {
			t.Fatal(err)
		}
		return dc.CreateJob{ExpectedDeviceRevision: device.Revision, ProjectID: e.project.ProjectID, WorkspaceAlias: device.WorkspaceAlias, Job: canonical, JobDigest: shaOf(canonical)}, spec
	}
	create := func(name string) (dc.JobView, localassistant.Job) {
		t.Helper()
		in, spec := newJob(name)
		var job dc.JobView
		web.must(http.MethodPost, devicePath+"/jobs", "create-"+name, in, &job, 200)
		var replay dc.JobView
		web.must(http.MethodPost, devicePath+"/jobs", "create-"+name, in, &replay, 200)
		if replay.ID != job.ID || replay.OperationID != job.OperationID || replay.Fence != nil {
			t.Fatal("job create replay changed identity or dispatched before inspection")
		}
		return job, spec
	}
	inspect := func(job dc.JobView, spec localassistant.Job) dc.JobView {
		t.Helper()
		in := dc.InspectRequest{Inspection: dc.Inspection{Schema: localassistant.InspectionSchema, JobID: spec.JobID, Action: spec.Action, JobDigest: job.JobDigest, ApprovalDigest: strings.Repeat("b", 64), WorkspaceBinding: device.WorkspaceBinding, InputSHA256: shaOf(spec.Input), Paths: map[string]string{"directory": spec.Directory, "output": spec.Output}, TimeoutSeconds: 3600, Warnings: []string{}}}
		var out dc.JobView
		worker.must(http.MethodPost, devicePath+"/jobs/"+string(job.ID)+"/inspect", "inspect-"+string(job.ID), in, &out, 200)
		if out.State != "awaiting_approval" || out.Fence != nil {
			t.Fatal("inspection bypassed the second human approval")
		}
		return out
	}
	approveJob := func(job dc.JobView) dc.JobView {
		t.Helper()
		in := dc.ApproveJob{ExpectedRevision: job.Revision, JobDigest: job.JobDigest, ApprovalDigest: job.ApprovalDigest, WorkspaceBinding: job.WorkspaceBinding}
		var out dc.JobView
		web.must(http.MethodPost, devicePath+"/jobs/"+string(job.ID)+"/approve", "approve-"+string(job.ID), in, &out, 200)
		return out
	}

	// No process has run a queued task, so cancellation records a real stop.
	queued, _ := create("control-queued-cancel")
	web.must(http.MethodPost, devicePath+"/jobs/"+string(queued.ID)+"/cancel", "cancel-queued", dc.Control{ExpectedRevision: queued.Revision}, &queued, 200)
	if queued.State != "cancelled" || !queued.Stopped || queued.Fence != nil {
		t.Fatal("queued cancel invented an attempt or failed to confirm no execution")
	}
	job, spec := create("control-one")
	deny(readOnly, http.MethodPost, devicePath+"/jobs", "no-organize", func() dc.CreateJob { x, _ := newJob("no-organize"); return x }(), errcode.Forbidden)
	job = inspect(job, spec)
	job = approveJob(job)

	// A distinct independent device session bound to the same real workspace
	// cannot obtain a concurrent or replacement attempt.
	secondSession := shortSession()
	secondWorker := backupBrowser{t: t, base: base, token: secondSession.Token}
	secondClaimInput := claimInput
	secondClaimInput.Alias = "synthetic-competing"
	var competingClaim dc.ClaimResponse
	secondWorker.must(http.MethodPost, "devices/claims", "claim-competitor", secondClaimInput, &competingClaim, 200)
	competingPath := "devices/" + string(competingClaim.Device.ID)
	var competingDevice dc.Device
	web.must(http.MethodPost, competingPath+"/approve", "approve-competitor", dc.ApproveDevice{ExpectedRevision: competingClaim.Device.Revision, ClaimCode: competingClaim.ClaimCode, WorkspaceAlias: competingClaim.Device.WorkspaceAlias, WorkspaceBinding: competingClaim.Device.WorkspaceBinding}, &competingDevice, 200)
	originalDevice, originalPath, originalWorker := device, devicePath, worker
	device, devicePath, worker = competingDevice, competingPath, secondWorker
	competingJob, competingSpec := create("control-competitor")
	competingJob = inspect(competingJob, competingSpec)
	competingJob = approveJob(competingJob)
	device, devicePath, worker = originalDevice, originalPath, originalWorker
	var first dc.Poll
	worker.must(http.MethodPost, devicePath+"/poll", "claim-original", struct{}{}, &first, 200)
	if first.Stop || first.Job == nil || first.Job.ID != job.ID || first.Job.Fence == nil || first.Job.State != "running" {
		t.Fatal("approved device did not claim the T06 attempt")
	}
	deny(secondWorker, http.MethodPost, competingPath+"/poll", "concurrent-claim-denied", struct{}{}, errcode.ResourceBusy)

	var workerRead dc.JobView
	worker.must(http.MethodGet, devicePath+"/jobs/"+string(job.ID), "", nil, &workerRead, 200)
	if workerRead.ID != job.ID || workerRead.Fence == nil || *workerRead.Fence != *first.Job.Fence {
		t.Fatal("bound worker cannot read its original attempt")
	}
	deny(secondWorker, http.MethodGet, devicePath+"/jobs/"+string(job.ID), "", nil, errcode.NotFound)
	deny(secondWorker, http.MethodGet, "devices?project_id="+string(e.project.ProjectID), "", nil, errcode.NotFound)
	deny(secondWorker, http.MethodPost, devicePath+"/revoke", "worker-cannot-manage-other-device", dc.Control{ExpectedRevision: device.Revision}, errcode.NotFound)
	deny(worker, http.MethodPost, competingPath+"/jobs/"+string(competingJob.ID)+"/approve", "worker-cannot-approve-other-device", dc.ApproveJob{ExpectedRevision: competingJob.Revision, JobDigest: competingJob.JobDigest, ApprovalDigest: competingJob.ApprovalDigest, WorkspaceBinding: competingJob.WorkspaceBinding}, errcode.NotFound)
	deny(worker, http.MethodGet, competingPath+"/jobs/"+string(job.ID), "", nil, errcode.NotFound)
	var second dc.Poll
	worker.must(http.MethodPost, devicePath+"/poll", "poll-same-running", struct{}{}, &second, 200)
	if second.Job == nil || *second.Job.Fence != *first.Job.Fence {
		t.Fatal("a second poll created another attempt or fence")
	}
	badFence := *first.Job.Fence
	badFence.LeaseFence++
	deny(worker, http.MethodPost, devicePath+"/jobs/"+string(job.ID)+"/heartbeat", "bad-fence", dc.LeaseRequest{Fence: badFence}, errcode.LeaseStale)
	deny(web, http.MethodPost, devicePath+"/jobs/"+string(job.ID)+"/heartbeat", "browser-not-worker", dc.LeaseRequest{Fence: *first.Job.Fence}, errcode.NotFound)
	var current dc.JobView
	web.must(http.MethodGet, devicePath+"/jobs/"+string(job.ID), "", nil, &current, 200)
	web.must(http.MethodPost, devicePath+"/jobs/"+string(job.ID)+"/cancel", "cancel-original", dc.Control{ExpectedRevision: current.Revision}, &current, 200)
	var stopped dc.Poll
	worker.must(http.MethodPost, devicePath+"/jobs/"+string(job.ID)+"/heartbeat", "cancel-stop", dc.LeaseRequest{Fence: *first.Job.Fence}, &stopped, 200)
	if !stopped.Stop || stopped.Job == nil || !stopped.Job.CancelRequested {
		t.Fatal("cancellation did not stop the original lease")
	}
	resultFor := func(j dc.JobView, spec localassistant.Job, status string) json.RawMessage {
		t.Helper()
		out := localassistant.Result{Schema: localassistant.ResultSchema, JobID: spec.JobID, Action: spec.Action, JobDigest: j.JobDigest, ApprovalDigest: j.ApprovalDigest, Status: status, ExitCode: 0, StartedAt: e.clk.Now(), FinishedAt: e.clk.Now()}
		if status == "cancelled" {
			out.ExitCode = 130
		}
		if status == "completed" {
			out.Result, _ = json.Marshal(map[string]string{"schema": "lantai.local-assistant-result-ref/v1", "job_id": string(j.ID), "result_sha256": strings.Repeat("e", 64)})
		}
		raw, _ := json.Marshal(out)
		return raw
	}

	invalidResult := resultFor(*first.Job, spec, "completed")
	var invalidObject map[string]any
	_ = json.Unmarshal(invalidResult, &invalidObject)
	invalidObject["result"] = map[string]string{"schema": "lantai.local-assistant-result-ref/v1", "job_id": string(ids.New()), "result_sha256": strings.Repeat("e", 64)}
	badResult, _ := json.Marshal(invalidObject)
	deny(worker, http.MethodPost, devicePath+"/jobs/"+string(job.ID)+"/result", "wrong-result-binding", dc.ResultRequest{Fence: *first.Job.Fence, Result: badResult, Stopped: true}, errcode.RefMismatch)
	invalidObject["result"] = map[string]string{"schema": "lantai.local-assistant-result-ref/v1", "job_id": string(job.ID), "result_sha256": strings.Repeat("e", 64), "directory": "/private/local/path"}
	badResult, _ = json.Marshal(invalidObject)
	deny(worker, http.MethodPost, devicePath+"/jobs/"+string(job.ID)+"/result", "private-result-fields", dc.ResultRequest{Fence: *first.Job.Fence, Result: badResult, Stopped: true}, errcode.RefMismatch)
	var done dc.JobView
	worker.must(http.MethodPost, devicePath+"/jobs/"+string(job.ID)+"/result", "result-cancelled", dc.ResultRequest{Fence: *first.Job.Fence, Result: resultFor(*first.Job, spec, "cancelled"), Stopped: true}, &done, 200)
	if done.State != "cancelled" || !done.Stopped {
		t.Fatal("confirmed cancellation was not retained")
	}
	lateJob, lateSpec := create("control-late")
	lateJob = inspect(lateJob, lateSpec)
	lateJob = approveJob(lateJob)
	var latePoll dc.Poll
	worker.must(http.MethodPost, devicePath+"/poll", "claim-late", struct{}{}, &latePoll, 200)
	if latePoll.Job == nil || latePoll.Job.Fence == nil {
		t.Fatal("late test did not obtain a real T06 fence")
	}
	e.clk.Advance(31 * time.Second)
	worker.must(http.MethodPost, devicePath+"/poll", "observe-expired", struct{}{}, &stopped, 200)
	if !stopped.Stop || stopped.Job == nil || stopped.Job.State != "needs_reconciliation" {
		t.Fatal("expired attempt was silently requeued")
	}
	worker.must(http.MethodPost, devicePath+"/jobs/"+string(lateJob.ID)+"/result", "late-success-is-unknown", dc.ResultRequest{Fence: *latePoll.Job.Fence, Result: resultFor(*latePoll.Job, lateSpec, "completed"), Stopped: true}, &done, 200)
	if done.State != "needs_reconciliation" || done.FailureCode != "OPERATION_NEEDS_RECONCILIATION" {
		t.Fatal("late completed report was accepted as success")
	}

	web.must(http.MethodPost, devicePath+"/jobs/"+string(lateJob.ID)+"/cancel", "cancel-unknown", dc.Control{ExpectedRevision: done.Revision}, &done, 200)
	if done.State != "needs_reconciliation" || !done.CancelRequested {
		t.Fatal("cancel erased unknown external effects")
	}
	deny(secondWorker, http.MethodPost, competingPath+"/poll", "unknown-blocks-new-executor", struct{}{}, errcode.ResourceBusy)
	oldID, oldOperation, oldFence := done.ID, done.OperationID, *done.Fence
	if err := servers.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	a = reopenApplication(t, e)
	servers = backupHTTPServers(t, a)
	base = "http://" + servers.Addresses.API
	web.base = base
	worker.base = base
	web.must(http.MethodGet, devicePath+"/jobs/"+string(oldID), "", nil, &current, 200)
	if current.ID != oldID || current.OperationID != oldOperation || current.Fence == nil || *current.Fence != oldFence || current.State != "needs_reconciliation" {
		t.Fatal("core restart changed the original job, operation, fence or unknown state")
	}
	// The owner port must inherit an existing security guard, as batch/check
	// does, rather than reacquiring it and violating lock order.
	owner := e.login().Context
	if err := e.id.BeginRead(ctx, owner, identity.ActDeviceRead, authz.Resource{ProjectID: e.project.ProjectID}, func(c context.Context, _ authz.Decision) error { _, err := a.DeviceJob(c, owner, oldID); return err }); err != nil {
		t.Fatal(err)
	}
	var revoked dc.Device
	web.must(http.MethodPost, devicePath+"/revoke", "revoke-device", dc.Control{ExpectedRevision: device.Revision}, &revoked, 200)
	if revoked.State != "revoked" {
		t.Fatal("device revocation was not durable")
	}
	deny(worker, http.MethodPost, devicePath+"/poll", "revoked-poll", struct{}{}, errcode.TokenRevoked)
	web.must(http.MethodGet, "whoami", "", nil, nil, 200)
	_, raw, _ := web.request(http.MethodGet, devicePath+"/jobs/"+string(oldID), "", nil)
	for _, secret := range []string{claim.ClaimCode, deviceSession.Token, web.cookie.Value, web.csrf, "claim_hash"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("device view leaked claim or session credentials")
		}
	}

	// Live role changes are rechecked rather than being copied into the pairing.
	secondWorker.base = base
	e.setRole(deviceSession.Context.PrincipalID, identity.RoleContributor, false)
	deny(secondWorker, http.MethodPost, competingPath+"/poll", "membership-revoked", struct{}{}, errcode.NotFound)
	e.setRole(deviceSession.Context.PrincipalID, identity.RoleContributor, true)
	e.clk.Advance(16 * time.Minute)
	deny(secondWorker, http.MethodPost, competingPath+"/poll", "device-session-expired", struct{}{}, errcode.TokenExpired)
	var expired dc.Device
	web.must(http.MethodGet, competingPath, "", nil, &expired, 200)
	if expired.State != "expired" {
		t.Fatal("expired device still advertised active")
	}
	t.Log("real HTTP/T01: independent project session, two explicit approvals, stable T06 fence, cancellation, expiry, restart and device-only session revocation verified")
}
