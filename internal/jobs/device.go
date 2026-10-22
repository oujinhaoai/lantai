package jobs

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/oujinhaoai/lantai/internal/commands"
	"slices"
	"time"

	ae "github.com/oujinhaoai/lantai/internal/contract/agentexec"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	dc "github.com/oujinhaoai/lantai/internal/contract/device"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	tc "github.com/oujinhaoai/lantai/internal/contract/tasks"
	"github.com/oujinhaoai/lantai/internal/node"
	"github.com/oujinhaoai/lantai/internal/workflow"
)

func commandsReadSecurity() commands.Request { return commands.Request{Security: commands.ModeShared} }

func deviceView(j Job) dc.JobView {
	out := dc.JobView{ID: j.ID, OperationID: j.OperationID, ProjectID: j.Request.ProjectID, State: j.State, Revision: j.Revision, LeaseExpiresAt: j.ExpiresAt, CancelRequested: j.CancelRequested, FailureCode: j.Failure}
	if j.Device != nil {
		out.JobData = *j.Device
	}
	if j.Attempt != nil {
		f := j.Attempt.Fence
		out.Fence = &f
	}
	return out
}
func (s *Service) nodeDevices() (deviceNodes, error) {
	p, ok := s.d.Nodes.(deviceNodes)
	if !ok {
		return nil, errcode.New(errcode.UnsupportedCapability, "device authority unavailable")
	}
	return p, nil
}
func (s *Service) loadDeviceJob(ctx context.Context, id, device ids.ID) (Job, error) {
	j, err := s.load(ctx, id)
	if err != nil {
		return j, err
	}
	if j.Device == nil || device != "" && j.Device.DeviceID != device {
		return Job{}, errcode.New(errcode.NotFound, "")
	}
	return j, nil
}
func (s *Service) webDevice(ctx context.Context, w authz.Context, id ids.ID, manage bool) (dc.Device, error) {
	p, err := s.nodeDevices()
	if err != nil {
		return dc.Device{}, err
	}
	return p.OwnedDevice(ctx, w, id, manage)
}
func (s *Service) workerDevice(ctx context.Context, w authz.Context, id ids.ID) (dc.Device, error) {
	p, err := s.nodeDevices()
	if err != nil {
		return dc.Device{}, err
	}
	return p.WorkerDevice(ctx, w, id)
}

// DeviceJob is the current permission-protected owner port used by both HTTP
// and server-side batch acceptance. It never trusts an archived client status.
func (s *Service) DeviceJob(ctx context.Context, w authz.Context, id ids.ID) (dc.JobView, error) {
	if !s.d.Gate.HoldsSecurity(ctx) {
		locked, h, err := s.d.Gate.Coordinator().Acquire(ctx, commandsReadSecurity())
		if err != nil {
			return dc.JobView{}, err
		}
		defer h.Release()
		ctx = locked
	}
	j, err := s.loadDeviceJob(ctx, id, "")
	if err != nil {
		return dc.JobView{}, err
	}
	d, err := s.webDevice(ctx, w, j.Device.DeviceID, false)
	// The independently bound worker may reconcile this precise original job.
	// This grants no discovery, pairing, approval or control privilege.
	if errcode.CodeOf(err) == errcode.NotFound {
		d, err = s.workerDevice(ctx, w, j.Device.DeviceID)
	}
	if err != nil {
		return dc.JobView{}, err
	}
	view := deviceView(j)
	if (j.State == "running" || j.State == "cancelling") && (d.State != "active" || s.deviceLeaseExpired(j)) {
		view.State = "needs_reconciliation"
		view.FailureCode = "LEASE_STALE"
	}
	return view, nil
}
func (s *Service) CreateDeviceJob(ctx context.Context, w authz.Context, key string, id ids.ID, in dc.CreateJob) (dc.JobView, error) {
	d, err := s.webDevice(ctx, w, id, true)
	if err != nil {
		return dc.JobView{}, err
	}
	base := Job{Request: workflow.JobRequest{ProjectID: d.ProjectID}}
	ctx, release, err := s.lock(ctx, base)
	if err != nil {
		return dc.JobView{}, err
	}
	defer release()
	d, err = s.webDevice(ctx, w, id, true)
	if err != nil {
		return dc.JobView{}, err
	}
	if in.ProjectID != d.ProjectID || in.WorkspaceAlias != d.WorkspaceAlias {
		return dc.JobView{}, errcode.New(errcode.RefMismatch, "")
	}
	spec, canonical, err := validateLocalJob(in.Job, in.JobDigest)
	if err != nil {
		return dc.JobView{}, err
	}
	if !slices.Contains(d.Actions, spec.Action) {
		return dc.JobView{}, errcode.New(errcode.Forbidden, "action is outside the device's explicitly selected allowlist")
	}
	if err = s.deviceActionAuth(ctx, w, d.ProjectID, spec.Action); err != nil {
		return dc.JobView{}, err
	}
	if err = s.deviceRefs(ctx, d, spec.Input); err != nil {
		return dc.JobView{}, err
	}
	if err = s.deviceRefs(ctx, d, spec.Report); err != nil {
		return dc.JobView{}, err
	}
	cc, err := s.command(ctx, w, key, "jobs.device_create", base, struct {
		Device ids.ID
		Input  dc.CreateJob
	}{id, in})
	if err != nil {
		return dc.JobView{}, err
	}
	if old, e := s.replay(ctx, cc); old != nil || e != nil {
		if old != nil {
			return deviceView(*old), e
		}
		return dc.JobView{}, e
	}
	if d.State != "active" || d.Revision != in.ExpectedDeviceRevision {
		return dc.JobView{}, errcode.New(errcode.PreconditionFailed, "active device revision required")
	}
	jobID, err := s.d.IDs.New()
	if err != nil {
		return dc.JobView{}, err
	}
	j := Job{ID: jobID, OperationID: cc.OperationID, Request: workflow.JobRequest{OperationID: cc.OperationID, ProjectID: d.ProjectID, Processor: "local-assistant"}, State: "queued", Epoch: cc.RecoveryEpoch, Device: &dc.JobData{DeviceID: id, OwnerID: w.PrincipalID, WorkspaceAlias: d.WorkspaceAlias, WorkspaceBinding: d.WorkspaceBinding, Job: canonical, JobDigest: in.JobDigest}}
	j, err = s.save(ctx, cc, j)
	return deviceView(j), err
}
func (s *Service) DeviceJobs(ctx context.Context, w authz.Context, id, after ids.ID, limit int) (dc.JobPage, error) {
	if after != "" && !after.Valid() || limit < 1 || limit > 100 {
		return dc.JobPage{}, deviceInvalid("bounded stable job page required")
	}
	ctx, h, err := s.d.Gate.Coordinator().Acquire(ctx, commandsReadSecurity())
	if err != nil {
		return dc.JobPage{}, err
	}
	defer h.Release()
	d, err := s.webDevice(ctx, w, id, false)
	if err != nil {
		return dc.JobPage{}, err
	}
	rows, err := s.d.DB.QueryContext(ctx, `SELECT record FROM jobs_jobs WHERE project_id=? AND json_extract(record,'$.device.device_id')=? AND job_id>? ORDER BY job_id LIMIT ?`, d.ProjectID, id, after, limit+1)
	if err != nil {
		return dc.JobPage{}, err
	}
	defer rows.Close()
	out := dc.JobPage{Items: []dc.JobView{}}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return dc.JobPage{}, err
		}
		var j Job
		if err = json.Unmarshal([]byte(raw), &j); err != nil {
			return dc.JobPage{}, err
		}
		if j.Device == nil || j.Device.OwnerID != w.PrincipalID {
			return dc.JobPage{}, errcode.New(errcode.NotFound, "")
		}
		v := deviceView(j)
		if (j.State == "running" || j.State == "cancelling") && (d.State != "active" || s.deviceLeaseExpired(j)) {
			v.State = "needs_reconciliation"
			v.FailureCode = "LEASE_STALE"
		}
		out.Items = append(out.Items, v)
	}
	if len(out.Items) > limit {
		out.Items = out.Items[:limit]
		out.NextCursor = string(out.Items[limit-1].ID)
	}
	return out, rows.Err()
}
func (s *Service) InspectDeviceJob(ctx context.Context, w authz.Context, key string, id, jobID ids.ID, in dc.InspectRequest) (dc.JobView, error) {
	j, err := s.loadDeviceJob(ctx, jobID, id)
	if err != nil {
		return dc.JobView{}, err
	}
	ctx, release, err := s.lock(ctx, j)
	if err != nil {
		return dc.JobView{}, err
	}
	defer release()
	d, err := s.workerDevice(ctx, w, id)
	if err != nil {
		return dc.JobView{}, err
	}
	j, err = s.loadDeviceJob(ctx, jobID, id)
	if err != nil {
		return dc.JobView{}, err
	}
	spec, _, err := validateLocalJob(j.Device.Job, j.Device.JobDigest)
	if err != nil {
		return dc.JobView{}, err
	}
	if err = s.deviceActionAuth(ctx, w, d.ProjectID, spec.Action); err != nil {
		return dc.JobView{}, err
	}
	i := in.Inspection
	timeout := spec.TimeoutSeconds
	if timeout == 0 {
		timeout = 3600
	}
	if i.Schema != "lantai.local-assistant-inspection/v1" || i.JobID != spec.JobID || i.Action != spec.Action || i.JobDigest != j.Device.JobDigest || !node.DeviceDigest.MatchString(i.ApprovalDigest) || i.WorkspaceBinding != d.WorkspaceBinding || !node.DeviceDigest.MatchString(i.InputSHA256) || i.Origin != spec.Origin || i.NetworkRequired != deviceNetwork(spec.Action) || i.SourceDeleted || i.RemoteWrites != (spec.Action == "import.apply") || i.TimeoutSeconds != timeout || len(i.Warnings) > 16 {
		return dc.JobView{}, errcode.New(errcode.RefMismatch, "inspection must bind the exact job, selected workspace, origin and action")
	}
	wantPaths := map[string]string{"directory": spec.Directory, "output": spec.Output}
	for k, p := range map[string]string{"state": spec.State, "destination": spec.Destination, "input_file": spec.InputFile, "report_file": spec.ReportFile} {
		if p != "" {
			wantPaths[k] = p
		}
	}
	a, _ := canonjson.CanonicalizeValue(i.Paths)
	b, _ := canonjson.CanonicalizeValue(wantPaths)
	if string(a) != string(b) {
		return dc.JobView{}, errcode.New(errcode.RefMismatch, "inspection paths changed the requested constraints")
	}
	for _, warning := range i.Warnings {
		if len(warning) > 512 {
			return dc.JobView{}, deviceInvalid("bounded public inspection warnings required")
		}
	}
	cc, err := s.command(ctx, w, key, "jobs.device_inspect", j, in)
	if err != nil {
		return dc.JobView{}, err
	}
	if old, e := s.replay(ctx, cc); old != nil || e != nil {
		if old != nil {
			return deviceView(*old), e
		}
		return dc.JobView{}, e
	}
	if j.State != "queued" || j.CancelRequested {
		return dc.JobView{}, errcode.New(errcode.InvalidStateTransition, "")
	}
	j.Device.Inspection = &i
	j.Device.ApprovalDigest = i.ApprovalDigest
	j.State = "awaiting_approval"
	j, err = s.save(ctx, cc, j)
	return deviceView(j), err
}
func (s *Service) ApproveDeviceJob(ctx context.Context, w authz.Context, key string, id, jobID ids.ID, in dc.ApproveJob) (dc.JobView, error) {
	j, err := s.loadDeviceJob(ctx, jobID, id)
	if err != nil {
		return dc.JobView{}, err
	}
	ctx, release, err := s.lock(ctx, j)
	if err != nil {
		return dc.JobView{}, err
	}
	defer release()
	d, err := s.webDevice(ctx, w, id, true)
	if err != nil {
		return dc.JobView{}, err
	}
	j, err = s.loadDeviceJob(ctx, jobID, id)
	if err != nil {
		return dc.JobView{}, err
	}
	cc, err := s.command(ctx, w, key, "jobs.device_approve", j, in)
	if err != nil {
		return dc.JobView{}, err
	}
	if old, e := s.replay(ctx, cc); old != nil || e != nil {
		if old != nil {
			return deviceView(*old), e
		}
		return dc.JobView{}, e
	}
	if d.State != "active" || j.State != "awaiting_approval" || j.Revision != in.ExpectedRevision || j.CancelRequested || j.Device.Inspection == nil || in.JobDigest != j.Device.JobDigest || in.ApprovalDigest != j.Device.ApprovalDigest || in.WorkspaceBinding != j.Device.WorkspaceBinding {
		return dc.JobView{}, errcode.New(errcode.PreconditionFailed, "exact current device inspection must be explicitly approved")
	}
	spec, _, err := validateLocalJob(j.Device.Job, j.Device.JobDigest)
	if err != nil {
		return dc.JobView{}, err
	}
	if err = s.deviceActionAuth(ctx, w, d.ProjectID, spec.Action); err != nil {
		return dc.JobView{}, err
	}
	j.State = "ready"
	j, err = s.save(ctx, cc, j)
	return deviceView(j), err
}
func (s *Service) deviceLeaseExpired(j Job) bool {
	expiry, err := time.Parse(time.RFC3339Nano, j.ExpiresAt)
	return err != nil || !s.d.Clock.Now().Before(expiry)
}
func (s *Service) PollDevice(ctx context.Context, w authz.Context, key string, id ids.ID) (dc.Poll, error) {
	d, err := s.workerDevice(ctx, w, id)
	if err != nil {
		return dc.Poll{}, err
	}
	base := Job{Request: workflow.JobRequest{ProjectID: d.ProjectID}}
	ctx, release, err := s.lock(ctx, base)
	if err != nil {
		return dc.Poll{}, err
	}
	defer release()
	d, err = s.workerDevice(ctx, w, id)
	if err != nil {
		return dc.Poll{}, err
	}
	var raw string
	err = s.d.DB.QueryRowContext(ctx, `SELECT record FROM jobs_jobs WHERE project_id=? AND json_extract(record,'$.device.device_id')=? AND state IN ('running','cancelling','needs_reconciliation','queued','awaiting_approval','ready') ORDER BY CASE WHEN state IN ('running','cancelling','needs_reconciliation') THEN 0 ELSE 1 END,job_id LIMIT 1`, d.ProjectID, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return dc.Poll{DeviceID: id}, nil
	}
	if err != nil {
		return dc.Poll{}, err
	}
	var j Job
	if err = json.Unmarshal([]byte(raw), &j); err != nil {
		return dc.Poll{}, err
	}
	spec, _, err := validateLocalJob(j.Device.Job, j.Device.JobDigest)
	if err != nil {
		return dc.Poll{}, err
	}
	if err = s.deviceActionAuth(ctx, w, d.ProjectID, spec.Action); err != nil {
		return dc.Poll{}, err
	}
	if j.State == "running" || j.State == "cancelling" {
		if s.deviceLeaseExpired(j) || j.Epoch != w.RecoveryEpoch {
			cc, e := s.command(ctx, w, "expire-"+string(j.Attempt.ID), "jobs.device_expire", j, j.Attempt.Fence)
			if e != nil {
				return dc.Poll{}, e
			}
			j.State = "needs_reconciliation"
			j.Failure = "LEASE_STALE"
			j.Attempt.State = "needs_reconciliation"
			j.Attempt.Outcome = execution.InvocationUnresolved
			j.Attempt.Verdict = execution.VerdictUnknown
			j.Attempt.Termination.UnresolvedEffects = 1
			j, e = s.save(ctx, cc, j)
			if e != nil {
				return dc.Poll{}, e
			}
		}
	}
	if j.State != "ready" {
		v := deviceView(j)
		return dc.Poll{DeviceID: id, Job: &v, Stop: j.CancelRequested || j.State == "needs_reconciliation", Reason: j.Failure}, nil
	}
	cc, err := s.command(ctx, w, key, "jobs.device_claim", j, struct {
		ID       ids.ID
		Revision int64
	}{j.ID, j.Revision})
	if err != nil {
		return dc.Poll{}, err
	}
	if old, e := s.replay(ctx, cc); old != nil || e != nil {
		if e != nil {
			return dc.Poll{}, e
		}
		current, e := s.loadDeviceJob(ctx, old.ID, id)
		v := deviceView(current)
		return dc.Poll{DeviceID: id, Job: &v}, e
	}
	var busy int
	err = s.d.DB.QueryRowContext(ctx, `SELECT count(*) FROM jobs_jobs WHERE job_id<>? AND state IN ('running','cancelling','needs_reconciliation') AND json_extract(record,'$.device.workspace_binding')=?`, j.ID, d.WorkspaceBinding).Scan(&busy)
	if err != nil {
		return dc.Poll{}, err
	}
	if busy > 0 {
		return dc.Poll{}, errcode.New(errcode.ResourceBusy, "the workspace already has an active or unresolved T06 attempt")
	}
	if j.Epoch != cc.RecoveryEpoch || j.Device.Inspection == nil || j.Device.ApprovalDigest == "" {
		return dc.Poll{}, errcode.New(errcode.LeaseStale, "")
	}
	aid, err := s.d.IDs.New()
	if err != nil {
		return dc.Poll{}, err
	}
	j.Attempts++
	input := tc.InputSnapshot{Refs: []ids.PermanentRef{}}
	input.Digest, err = tc.SnapshotDigest(input.Refs)
	if err != nil {
		return dc.Poll{}, err
	}
	j.Attempt = &ae.JobAttempt{Contract: "lantai.job-attempt/v1", ID: aid, JobID: j.ID, OperationID: cc.OperationID, Revision: 1, Protocol: execution.LocalAssistant, Fence: ae.JobFence{JobID: j.ID, JobAttemptID: aid, LeaseFence: int64(j.Attempts), RecoveryEpoch: cc.RecoveryEpoch}, Input: input, State: "running", LocalExecution: &ae.LocalExecutionBinding{DeviceID: id, WorkspaceAlias: d.WorkspaceAlias, WorkspaceBinding: d.WorkspaceBinding, JobDigest: j.Device.JobDigest, ApprovalDigest: j.Device.ApprovalDigest}}
	j.WorkerID = w.PrincipalID
	j.SessionID = w.SessionID
	j.State = "running"
	expires := s.d.Clock.Now().Add(30 * time.Second)
	if w.ExpiresAt.Before(expires) {
		expires = w.ExpiresAt
	}
	j.ExpiresAt = clock.Format(expires)
	j, err = s.save(ctx, cc, j)
	v := deviceView(j)
	return dc.Poll{DeviceID: id, Job: &v}, err
}
func (s *Service) HeartbeatDeviceJob(ctx context.Context, w authz.Context, key string, id, jobID ids.ID, in dc.LeaseRequest) (dc.Poll, error) {
	j, err := s.loadDeviceJob(ctx, jobID, id)
	if err != nil {
		return dc.Poll{}, err
	}
	ctx, release, err := s.lock(ctx, j)
	if err != nil {
		return dc.Poll{}, err
	}
	defer release()
	if _, err = s.workerDevice(ctx, w, id); err != nil {
		return dc.Poll{}, err
	}
	j, err = s.loadDeviceJob(ctx, jobID, id)
	if err != nil {
		return dc.Poll{}, err
	}
	if j.Attempt == nil || j.Attempt.Fence != in.Fence || j.SessionID != w.SessionID || j.WorkerID != w.PrincipalID || j.Epoch != w.RecoveryEpoch {
		return dc.Poll{}, errcode.New(errcode.LeaseStale, "")
	}
	spec, _, err := validateLocalJob(j.Device.Job, j.Device.JobDigest)
	if err != nil {
		return dc.Poll{}, err
	}
	if err = s.deviceActionAuth(ctx, w, j.Request.ProjectID, spec.Action); err != nil {
		return dc.Poll{}, err
	}
	if j.CancelRequested || j.State != "running" || s.deviceLeaseExpired(j) {
		v := deviceView(j)
		return dc.Poll{DeviceID: id, Job: &v, Stop: true, Reason: "LEASE_STALE"}, nil
	}
	cc, err := s.command(ctx, w, key, "jobs.device_heartbeat", j, in)
	if err != nil {
		return dc.Poll{}, err
	}
	if old, e := s.replay(ctx, cc); old != nil || e != nil {
		if e != nil {
			return dc.Poll{}, e
		}
		v := deviceView(j)
		return dc.Poll{DeviceID: id, Job: &v}, nil
	}
	expires := s.d.Clock.Now().Add(30 * time.Second)
	if w.ExpiresAt.Before(expires) {
		expires = w.ExpiresAt
	}
	j.ExpiresAt = clock.Format(expires)
	j, err = s.save(ctx, cc, j)
	v := deviceView(j)
	return dc.Poll{DeviceID: id, Job: &v}, err
}
func (s *Service) CancelDeviceJob(ctx context.Context, w authz.Context, key string, id, jobID ids.ID, in dc.Control) (dc.JobView, error) {
	j, err := s.loadDeviceJob(ctx, jobID, id)
	if err != nil {
		return dc.JobView{}, err
	}
	ctx, release, err := s.lock(ctx, j)
	if err != nil {
		return dc.JobView{}, err
	}
	defer release()
	if _, err = s.webDevice(ctx, w, id, true); err != nil {
		return dc.JobView{}, err
	}
	j, err = s.loadDeviceJob(ctx, jobID, id)
	if err != nil {
		return dc.JobView{}, err
	}
	cc, err := s.command(ctx, w, key, "jobs.device_cancel", j, in)
	if err != nil {
		return dc.JobView{}, err
	}
	if old, e := s.replay(ctx, cc); old != nil || e != nil {
		if old != nil {
			return deviceView(*old), e
		}
		return dc.JobView{}, e
	}
	if j.Revision != in.ExpectedRevision {
		return dc.JobView{}, errcode.New(errcode.PreconditionFailed, "")
	}
	if slices.Contains([]string{"succeeded", "failed", "cancelled"}, j.State) {
		return dc.JobView{}, errcode.New(errcode.InvalidStateTransition, "")
	}
	j.CancelRequested = true
	if j.Attempt == nil {
		j.State = "cancelled"
		j.Device.Stopped = true
	} else if j.State != "needs_reconciliation" {
		j.State = "cancelling"
		j.Attempt.State = "cancelling"
	}
	j, err = s.save(ctx, cc, j)
	return deviceView(j), err
}
func (s *Service) ResultDeviceJob(ctx context.Context, w authz.Context, key string, id, jobID ids.ID, in dc.ResultRequest) (dc.JobView, error) {
	j, err := s.loadDeviceJob(ctx, jobID, id)
	if err != nil {
		return dc.JobView{}, err
	}
	ctx, release, err := s.lock(ctx, j)
	if err != nil {
		return dc.JobView{}, err
	}
	defer release()
	if _, err = s.workerDevice(ctx, w, id); err != nil {
		return dc.JobView{}, err
	}
	j, err = s.loadDeviceJob(ctx, jobID, id)
	if err != nil {
		return dc.JobView{}, err
	}
	if j.Attempt == nil || j.Attempt.Fence != in.Fence || j.SessionID != w.SessionID || j.WorkerID != w.PrincipalID || j.Epoch != w.RecoveryEpoch {
		return dc.JobView{}, errcode.New(errcode.LeaseStale, "")
	}
	cc, err := s.command(ctx, w, key, "jobs.device_result", j, in)
	if err != nil {
		return dc.JobView{}, err
	}
	if old, e := s.replay(ctx, cc); old != nil || e != nil {
		if old != nil {
			return deviceView(*old), e
		}
		return dc.JobView{}, e
	}
	if !slices.Contains([]string{"running", "cancelling", "needs_reconciliation"}, j.State) {
		return dc.JobView{}, errcode.New(errcode.InvalidStateTransition, "")
	}
	if err = safeDeviceJSON(in.Result); err != nil {
		return dc.JobView{}, err
	}
	var result localResult
	decoder := json.NewDecoder(bytes.NewReader(in.Result))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&result); err != nil {
		return dc.JobView{}, deviceInvalid("strict original local result envelope required")
	}
	if len(result.Result) > 0 {
		ref, e := decodeResultRef(result.Result)
		if e != nil || ref.JobID != string(jobID) {
			return dc.JobView{}, errcode.New(errcode.RefMismatch, "private result must bind the original remote job")
		}
	} else if result.Status == "completed" {
		return dc.JobView{}, deviceInvalid("completed result requires its private result reference")
	}

	spec, _, err := validateLocalJob(j.Device.Job, j.Device.JobDigest)
	if err != nil {
		return dc.JobView{}, err
	}
	if err = s.deviceActionAuth(ctx, w, j.Request.ProjectID, spec.Action); err != nil {
		return dc.JobView{}, err
	}
	if result.Schema != "lantai.local-assistant-result/v1" || result.JobID != spec.JobID || result.Action != spec.Action || result.JobDigest != j.Device.JobDigest || result.ApprovalDigest != j.Device.ApprovalDigest || result.SourceDeleted || result.AutomaticRetry || result.CredentialIssued || result.StartedAt.IsZero() || result.FinishedAt.Before(result.StartedAt) || !slices.Contains([]string{"completed", "partial", "cancelled", "failed", "unknown"}, result.Status) || result.ExitCode < 0 || result.ExitCode > 255 {
		return dc.JobView{}, errcode.New(errcode.RefMismatch, "result must retain the original approved job and actual status")
	}
	late := s.deviceLeaseExpired(j) || j.State == "needs_reconciliation"
	j.Device.Result = append(json.RawMessage(nil), in.Result...)
	j.Device.Stopped = in.Stopped
	j.Attempt.Termination.Confirmed = in.Stopped
	j.Attempt.Revision++
	j.Attempt.ResultDigest = digest.Of(in.Result)
	j.Attempt.Verdict = execution.VerdictUnknown
	if late || !in.Stopped || result.Status == "unknown" || result.Status == "partial" || j.CancelRequested && result.Status != "cancelled" {
		j.State = "needs_reconciliation"
		j.Failure = "OPERATION_NEEDS_RECONCILIATION"
		j.Attempt.State = "needs_reconciliation"
		j.Attempt.Outcome = execution.InvocationUnresolved
		j.Attempt.Termination.UnresolvedEffects = 1
	} else {
		j.Attempt.State = "completed"
		j.Attempt.Termination.UnresolvedEffects = 0
		switch {
		case result.Status == "completed" && result.ExitCode == 0 && !j.CancelRequested:
			j.State = "succeeded"
			j.Attempt.Outcome = execution.InvocationCompleted
		case result.Status == "cancelled":
			j.State = "cancelled"
			j.Attempt.Outcome = execution.InvocationCancelled
		default:
			j.State = "failed"
			j.Failure = "LOCAL_ACTION_FAILED"
			j.Attempt.Outcome = execution.InvocationRuntimeFault
		}
	}
	j, err = s.save(ctx, cc, j)
	return deviceView(j), err
}
