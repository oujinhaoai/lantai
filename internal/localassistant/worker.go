package localassistant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"slices"
	"time"

	"github.com/oujinhaoai/lantai/internal/client"
	ae "github.com/oujinhaoai/lantai/internal/contract/agentexec"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	wire "github.com/oujinhaoai/lantai/internal/contract/device"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

type WorkerObservation struct {
	DeviceID       ids.ID          `json:"device_id"`
	JobID          ids.ID          `json:"job_id,omitempty"`
	State          string          `json:"state"`
	CoreState      string          `json:"core_state,omitempty"`
	Code           string          `json:"code,omitempty"`
	LocalResultRef *LocalResultRef `json:"local_result_ref,omitempty"`
}
type workerPending struct {
	Job     wire.JobView       `json:"job"`
	Request wire.ResultRequest `json:"request"`
	Key     string             `json:"key"`
}
type workerLink struct {
	Schema       string         `json:"schema"`
	ConfigDigest string         `json:"config_digest"`
	Device       wire.Device    `json:"device"`
	ClaimKey     string         `json:"claim_key"`
	PollKey      string         `json:"poll_key,omitempty"`
	Current      *wire.JobView  `json:"current,omitempty"`
	Pending      *workerPending `json:"pending,omitempty"`
	Unresolved   *wire.JobView  `json:"unresolved,omitempty"`
}

func control(ctx context.Context, c *client.Client, path string, body any, key string, target any) error {
	response, err := c.Do(ctx, http.MethodPost, path, body, client.Options{IdempotencyKey: key})
	if err != nil {
		return invalid("device control request failed; preserve original local state")
	}
	if len(response.Body) == 0 {
		if target == nil {
			return nil
		}
		return invalid("device control response missing")
	}
	if target != nil && strict(response.Body, target) != nil {
		return invalid("device control response invalid")
	}
	return nil
}
func workerMeta(ctx context.Context, s *workerStore, c *client.Client) error {
	r, e := c.Do(ctx, http.MethodGet, "/api/v1/meta", nil, client.Options{})
	var m struct {
		APIVersion string `json:"api_version"`
		InstanceID string `json:"instance_id"`
	}
	if e != nil || json.Unmarshal(r.Body, &m) != nil || m.APIVersion != "v1" || m.InstanceID != s.config.InstanceID {
		return invalid("worker origin/instance metadata changed")
	}
	return nil
}
func (s *workerStore) validDevice(d wire.Device) bool {
	return d.ID.Valid() && string(d.ProjectID) == s.config.ProjectID && d.Alias == s.config.Alias && d.WorkspaceAlias == s.config.WorkspaceAlias && d.WorkspaceBinding == hash([]byte(s.workspace)) && len(d.Actions) == len(s.config.Actions) && slices.Equal(d.Actions, s.config.Actions) && d.ExpiresAt.After(time.Now())
}
func (s *workerStore) loadLink() (workerLink, error) {
	var link workerLink
	err := s.load("device.json", &link)
	if err != nil {
		return link, err
	}
	if link.Schema != "lantai.assistant-worker-link/v1" || link.ConfigDigest != s.digest {
		return link, invalid("worker config differs from original device binding")
	}
	return link, nil
}

// EnrollDevice sends a real HTTPS claim with the explicitly selected existing
// short project session. Only the return value contains the one-time code.
func EnrollDevice(ctx context.Context, configFile, workspace string) (wire.ClaimResponse, error) {
	var response wire.ClaimResponse
	ctx, release, e := lockWorkspace(ctx, workspace)
	if e != nil {
		return response, e
	}
	defer release()
	s, e := openWorker(configFile, workspace)
	if e != nil {
		return response, e
	}
	defer s.close()
	c, e := s.api()
	if e != nil {
		return response, e
	}
	defer c.Close()
	if e = workerMeta(ctx, s, c); e != nil {
		return response, e
	}
	link, e := s.loadLink()
	if errors.Is(e, os.ErrNotExist) {
		link = workerLink{Schema: "lantai.assistant-worker-link/v1", ConfigDigest: s.digest, ClaimKey: string(ids.New())}
	} else if e != nil {
		return response, e
	}
	if link.Device.ID.Valid() {
		return response, invalid("device already linked; reuse original binding or explicitly choose another private workspace")
	}
	if link.ClaimKey == "" {
		return response, invalid("original claim key missing")
	}
	if e = s.save("device.json", link); e != nil {
		return response, e
	}
	req := wire.Claim{ProjectID: ids.ID(s.config.ProjectID), Alias: s.config.Alias, WorkspaceAlias: s.config.WorkspaceAlias, WorkspaceBinding: hash([]byte(s.workspace)), Actions: s.config.Actions}
	if e = control(ctx, c, "/api/v1/devices/claims", req, link.ClaimKey, &response); e != nil {
		return response, e
	}
	if !s.validDevice(response.Device) {
		return wire.ClaimResponse{}, invalid("device claim binding invalid")
	}
	link.Device = response.Device
	if e = s.save("device.json", link); e != nil {
		return wire.ClaimResponse{}, e
	}
	return response, nil
}

// WorkDevice runs only while explicitly invoked. once processes one control
// observation (queued inspection, approved execution or pending-result replay).
func WorkDevice(ctx context.Context, configFile, workspace string, once bool) (observation WorkerObservation, err error) {
	ctx, release, err := lockWorkspace(ctx, workspace)
	if err != nil {
		return observation, err
	}
	defer release()
	s, err := openWorker(configFile, workspace)
	if err != nil {
		return observation, err
	}
	defer s.close()
	link, err := s.loadLink()
	if err != nil {
		return observation, invalid("device has not been linked with this original config")
	}
	if !s.validDevice(link.Device) {
		return observation, invalid("device binding expired or changed")
	}
	ctx, stopSession := context.WithDeadline(ctx, link.Device.ExpiresAt)
	defer stopSession()
	c, err := s.api()
	if err != nil {
		return observation, err
	}
	defer c.Close()
	if err = workerMeta(ctx, s, c); err != nil {
		return observation, err
	}
	observation.DeviceID = link.Device.ID
	for {
		if err = s.check(); err != nil {
			return observation, err
		}
		if ctx.Err() != nil {
			return observation, ctx.Err()
		}
		if !link.Device.ExpiresAt.After(time.Now()) {
			return observation, invalid("device session expired; preserve original state")
		}
		if link.Pending != nil {
			observation, err = s.deliver(ctx, c, &link)
			if err != nil || once || observation.State == "unknown" {
				return observation, err
			}
			continue
		}
		if link.Unresolved != nil {
			observation = WorkerObservation{DeviceID: link.Device.ID, JobID: link.Unresolved.ID, State: "unknown", CoreState: link.Unresolved.State, Code: "ASSISTANT_CORE_RECONCILIATION_REQUIRED"}
			response, e := c.Do(ctx, http.MethodGet, "/api/v1/devices/"+string(link.Device.ID)+"/jobs/"+string(link.Unresolved.ID), nil, client.Options{})
			var current wire.JobView
			if e != nil || strict(response.Body, &current) != nil || current.ID != link.Unresolved.ID || current.DeviceID != link.Device.ID || current.ProjectID != link.Unresolved.ProjectID || current.Fence == nil || link.Unresolved.Fence == nil || *current.Fence != *link.Unresolved.Fence || current.JobDigest != link.Unresolved.JobDigest || current.ApprovalDigest != link.Unresolved.ApprovalDigest {
				return observation, invalid("original unresolved core attempt cannot be reconciled")
			}
			if !slices.Contains([]string{"succeeded", "cancelled", "failed"}, current.State) {
				return observation, nil
			}
			link.Unresolved = nil
			if err = s.save("device.json", link); err != nil {
				return observation, err
			}
		}
		if link.Current != nil {
			// Crash/restart can never silently run the old action again. The retained
			// original fence/result is reconciled first; any resume requires a new human approval.
			out := Result{Schema: ResultSchema, Status: "unknown", ExitCode: 3, ErrorCode: "ASSISTANT_RESTART_RECONCILE_ORIGINAL_STATE", JobDigest: link.Current.JobDigest, ApprovalDigest: link.Current.ApprovalDigest, StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC()}
			var job Job
			_ = strict(link.Current.Job, &job)
			out.JobID = job.JobID
			out.Action = job.Action
			var retained workerArtifact
			if e := s.load(artifactName(string(link.Current.ID)), &retained); e == nil && retained.Schema == "lantai.assistant-worker-artifact/v1" && retained.ConfigDigest == s.digest && retained.JobID == string(link.Current.ID) && retained.ProjectID == s.config.ProjectID && retained.WorkspaceAlias == s.config.WorkspaceAlias && retained.Result.JobDigest == link.Current.JobDigest && retained.Result.ApprovalDigest == link.Current.ApprovalDigest {
				out = retained.Result
			}
			if err = s.queueResult(&link, *link.Current, out); err != nil {
				return observation, err
			}
			continue
		}
		if link.PollKey == "" {
			link.PollKey = string(ids.New())
			if err = s.save("device.json", link); err != nil {
				return observation, err
			}
		}
		var poll wire.Poll
		if err = control(ctx, c, "/api/v1/devices/"+string(link.Device.ID)+"/poll", struct{}{}, link.PollKey, &poll); err != nil {
			return observation, err
		}
		if poll.DeviceID != link.Device.ID {
			return observation, invalid("poll device identity mismatch")
		}
		link.PollKey = ""
		if err = s.save("device.json", link); err != nil {
			return observation, err
		}
		if poll.Stop {
			observation.State = "stopped"
			observation.Code = "ASSISTANT_SERVER_STOP"
			return observation, nil
		}
		if poll.Job == nil {
			observation.State = "idle"
		} else {
			job := *poll.Job
			observation.JobID = job.ID
			path, inspection, e := s.inspectJob(job, link.Device.ID)
			if e != nil {
				observation.State = "blocked"
				observation.Code = "ASSISTANT_JOB_BINDING_REJECTED"
				return observation, e
			}
			switch job.State {
			case "queued":
				var inspected wire.JobView
				data, _ := json.Marshal(inspection)
				var dto wire.Inspection
				_ = json.Unmarshal(data, &dto)
				if err = control(ctx, c, jobRoute(link.Device.ID, job.ID, "inspect"), wire.InspectRequest{Inspection: dto}, "inspect-"+string(job.ID)+"-"+inspection.ApprovalDigest, &inspected); err != nil {
					return observation, err
				}
				if inspected.ID != job.ID || inspected.JobDigest != job.JobDigest || inspected.ApprovalDigest != inspection.ApprovalDigest {
					return observation, invalid("inspection acknowledgement binding invalid")
				}
				observation.State = "awaiting_approval"
			case "running":
				if job.ApprovalDigest != inspection.ApprovalDigest || job.Fence == nil || !validFence(*job.Fence, job.ID) || job.CancelRequested {
					return observation, invalid("job is not approved with a current execution fence")
				}
				link.Current = &job
				if err = s.save("device.json", link); err != nil {
					return observation, err
				}
				out := s.executeLeased(ctx, c, job, path, inspection)
				if err = s.queueResult(&link, job, out); err != nil {
					return observation, err
				}
				observation, err = s.deliver(ctx, c, &link)
				if err != nil {
					return observation, err
				}
				if observation.State == "unknown" {
					return observation, nil
				}
			case "awaiting_approval", "ready":
				observation.State = job.State
			default:
				observation.State = job.State
			}
		}
		if once {
			return observation, nil
		}
		timer := time.NewTimer(time.Duration(s.config.PollSeconds) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return observation, ctx.Err()
		case <-timer.C:
		}
	}
}
func jobRoute(deviceID, jobID ids.ID, action string) string {
	return "/api/v1/devices/" + string(deviceID) + "/jobs/" + string(jobID) + "/" + action
}
func validFence(f ae.JobFence, id ids.ID) bool {
	return f.JobID == id && f.JobID.Valid() && f.JobAttemptID.Valid() && f.LeaseFence > 0 && f.RecoveryEpoch >= 0
}
func (s *workerStore) inspectJob(remote wire.JobView, deviceID ids.ID) (string, Inspection, error) {
	fail := func() (string, Inspection, error) {
		return "", Inspection{}, invalid("remote job violates local project/alias/action/path/approval policy")
	}
	if !remote.ID.Valid() || remote.DeviceID != deviceID || string(remote.ProjectID) != s.config.ProjectID || remote.WorkspaceAlias != s.config.WorkspaceAlias || remote.WorkspaceBinding != hash([]byte(s.workspace)) {
		return fail()
	}
	var job Job
	if strict(remote.Job, &job) != nil || !slices.Contains(s.config.Actions, job.Action) || sensitiveJSON(remote.Job) {
		return fail()
	}
	canonical, e := canonjsonJob(remote.Job)
	if e != nil || hash(canonical) != remote.JobDigest {
		return fail()
	}
	if network(job.Action) && job.Origin != s.config.Origin {
		return fail()
	}
	for _, refRaw := range [][]byte{job.Input, job.Report} {
		if ref, isRef, e := decodeLocalRef(refRaw); e != nil {
			return fail()
		} else if isRef {
			a, e := readArtifact(s.workspace, ref)
			if e != nil || a.ConfigDigest != s.digest || a.ProjectID != s.config.ProjectID || a.WorkspaceAlias != s.config.WorkspaceAlias {
				return fail()
			}
		}
	}
	if job.InputFile != "" || job.ReportFile != "" {
		return fail()
	} // remote paths cannot select arbitrary private input files
	if slices.Contains([]string{"import.apply", "import.verify", "references.copy", "references.rollback"}, job.Action) {
		if _, isRef, e := decodeLocalRef(job.Input); e != nil || !isRef {
			return fail()
		}
	}
	if job.Action == "import.verify" {
		if _, isRef, e := decodeLocalRef(job.Report); e != nil || !isRef {
			return fail()
		}
	}
	roots := s.config.Roots
	directoryRoot := roots.Directory
	if job.Action == "references.rollback" {
		directoryRoot = roots.Destination
	}
	for _, pair := range [][2]string{{job.Directory, directoryRoot}, {job.State, roots.State}, {job.Destination, roots.Destination}, {job.Output, roots.Output}} {
		if pair[0] == "" {
			continue
		}
		actual, e := s.path(pair[0], true)
		if e != nil {
			return fail()
		}
		root, e := s.path(pair[1], true)
		if e != nil || !within(root, actual) {
			return fail()
		}
	}
	name := "job-" + string(remote.ID) + ".json"
	path, e := s.privatePath(name)
	if e != nil {
		return fail()
	}
	if old, e := client.ReadPrivate(path); e == nil {
		if !sameJSON(old, remote.Job) {
			return fail()
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return fail()
	} else if e = s.save(name, remote.Job); e != nil {
		return fail()
	}
	inspection, e := InspectFile(path, s.workspace)
	if e != nil {
		return "", inspection, e
	}
	// Project identifiers embedded in a local import plan cannot exceed the one
	// explicitly configured project, even with a compromised control response.
	p, e := prepare(path, s.workspace)
	if e != nil {
		return fail()
	}
	defer p.root.Close()
	if job.Action == "import.plan" {
		var in client.ImportInput
		if strict(p.input, &in) != nil {
			return fail()
		}
		for _, a := range in.Assets {
			if a.Push.ProjectID != s.config.ProjectID {
				return fail()
			}
		}
	}
	if job.Action == "import.apply" || job.Action == "import.verify" {
		var in client.ImportPlan
		if strict(p.input, &in) != nil {
			return fail()
		}
		for _, a := range in.Input.Assets {
			if a.Push.ProjectID != s.config.ProjectID {
				return fail()
			}
		}
		ref, _, _ := decodeLocalRef(job.Input)
		bindingName := "plan-state-" + ref.JobID + ".json"
		var binding struct {
			State string `json:"state"`
		}
		e = s.load(bindingName, &binding)
		if errors.Is(e, os.ErrNotExist) {
			if job.Action != "import.apply" {
				return fail()
			}
			binding.State = job.State
			if remote.State == "running" {
				if e = s.save(bindingName, binding); e != nil {
					return fail()
				}
			}
		} else if e != nil || binding.State != job.State {
			return fail()
		}
	}
	return path, inspection, nil
}
func canonjsonJob(raw []byte) ([]byte, error) { return canonjson.Canonicalize(raw) }
func (s *workerStore) queueResult(link *workerLink, job wire.JobView, out Result) error {
	artifact := workerArtifact{Schema: "lantai.assistant-worker-artifact/v1", JobID: string(job.ID), ConfigDigest: s.digest, ProjectID: s.config.ProjectID, WorkspaceAlias: s.config.WorkspaceAlias, Result: out}
	if e := s.save(artifactName(string(job.ID)), artifact); e != nil {
		return e
	}
	safe := out
	if len(out.Result) > 0 {
		ref := LocalResultRef{Schema: LocalResultRefSchema, JobID: string(job.ID), ResultSHA256: hash(out.Result)}
		safe.Result, _ = json.Marshal(ref)
	}
	encoded, e := json.Marshal(safe)
	if e != nil || sensitiveJSON(encoded) {
		return invalid("worker result cannot be safely delivered")
	}
	if job.Fence == nil {
		return invalid("original result fence missing")
	}
	link.Pending = &workerPending{Job: job, Request: wire.ResultRequest{Fence: *job.Fence, Result: encoded, Stopped: true}, Key: "result-" + string(job.ID) + "-" + string(job.Fence.JobAttemptID)}
	return s.save("device.json", link)
}
func (s *workerStore) deliver(ctx context.Context, c *client.Client, link *workerLink) (WorkerObservation, error) {
	p := link.Pending
	if p == nil {
		return WorkerObservation{}, invalid("pending result missing")
	}
	observation := WorkerObservation{DeviceID: link.Device.ID, JobID: p.Job.ID, State: "unknown", Code: "ASSISTANT_RESULT_DELIVERY_PENDING"}
	var accepted wire.JobView
	delivery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if e := control(delivery, c, jobRoute(link.Device.ID, p.Job.ID, "result"), p.Request, p.Key, &accepted); e != nil {
		return observation, e
	}
	if accepted.ID != p.Job.ID || accepted.DeviceID != link.Device.ID || accepted.ProjectID != p.Job.ProjectID || accepted.Fence == nil || *accepted.Fence != p.Request.Fence || accepted.JobDigest != p.Job.JobDigest || accepted.ApprovalDigest != p.Job.ApprovalDigest || !accepted.Stopped || !sameJSON(accepted.Result, p.Request.Result) {
		return observation, invalid("result acknowledgement binding mismatch")
	}
	var out Result
	if strict(p.Request.Result, &out) != nil {
		return observation, invalid("pending result invalid")
	}
	observation.State = out.Status
	observation.CoreState = accepted.State
	observation.Code = out.ErrorCode
	if accepted.State == "needs_reconciliation" || accepted.State == "running" || accepted.State == "cancelling" || out.Status == "unknown" || out.Status == "partial" || (out.Status == "completed" && accepted.State != "succeeded") {
		observation.State = "unknown"
		if observation.Code == "" || out.Status == "completed" {
			observation.Code = "ASSISTANT_CORE_RECONCILIATION_REQUIRED"
		}
		link.Unresolved = &accepted
	}
	if ref, isRef, _ := decodeLocalRef(out.Result); isRef {
		observation.LocalResultRef = &ref
	}
	link.Pending = nil
	link.Current = nil
	if e := s.save("device.json", link); e != nil {
		return observation, e
	}
	return observation, nil
}
func (s *workerStore) checkHeartbeat(ctx context.Context, c *client.Client, job wire.JobView, expires time.Time) (time.Time, error) {
	if !expires.After(time.Now()) {
		return time.Time{}, invalid("lease expired")
	}
	request, stop := context.WithDeadline(ctx, expires)
	defer stop()
	var poll wire.Poll
	err := control(request, c, jobRoute(job.DeviceID, job.ID, "heartbeat"), wire.LeaseRequest{Fence: *job.Fence}, string(ids.New()), &poll)
	if err != nil || poll.Stop || poll.DeviceID != job.DeviceID || poll.Job == nil || poll.Job.State != "running" || poll.Job.ID != job.ID || poll.Job.Fence == nil || *poll.Job.Fence != *job.Fence || poll.Job.CancelRequested || poll.Job.JobDigest != job.JobDigest || poll.Job.ApprovalDigest != job.ApprovalDigest {
		return time.Time{}, invalid("lease lost or revoked")
	}
	next, err := time.Parse(time.RFC3339Nano, poll.Job.LeaseExpiresAt)
	if err != nil || !next.After(time.Now()) {
		return time.Time{}, invalid("lease expired")
	}
	if err = s.check(); err != nil {
		return time.Time{}, err
	}
	return next, nil
}
func (s *workerStore) checkWorksetProject(ctx context.Context, c *client.Client, path string, action string) error {
	if action != "workset.plan" && action != "workset.pull" {
		return nil
	}
	p, err := prepare(path, s.workspace)
	if err != nil {
		return err
	}
	defer p.root.Close()
	in, err := client.DecodeWorkset(bytes.NewReader(p.input))
	if err != nil {
		return err
	}
	for _, entry := range in.Entries {
		r, e := c.Do(ctx, http.MethodGet, "/api/v1/assets/"+entry.AssetID+"/versions/"+entry.VersionID+"?view=full", nil, client.Options{})
		if e != nil {
			return e
		}
		var exact struct {
			Version struct {
				ProjectID string `json:"project_id"`
			} `json:"version"`
			Manifest struct {
				InstanceID string `json:"instance_id"`
				ProjectID  string `json:"project_id"`
			} `json:"manifest"`
		}
		if json.Unmarshal(r.Body, &exact) != nil || exact.Version.ProjectID != s.config.ProjectID || exact.Manifest.ProjectID != s.config.ProjectID || exact.Manifest.InstanceID != s.config.InstanceID {
			return invalid("workset exceeds configured project/instance")
		}
	}
	return nil
}
func (s *workerStore) executeLeased(parent context.Context, c *client.Client, job wire.JobView, path string, inspection Inspection) Result {
	unknown := func(code string) Result {
		return Result{Schema: ResultSchema, JobID: inspection.JobID, Action: inspection.Action, JobDigest: inspection.JobDigest, ApprovalDigest: inspection.ApprovalDigest, Status: "unknown", ExitCode: 3, ErrorCode: code, StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC()}
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	expires, err := time.Parse(time.RFC3339Nano, job.LeaseExpiresAt)
	if err == nil {
		expires, err = s.checkHeartbeat(ctx, c, job, expires)
	}
	if err != nil {
		return unknown("ASSISTANT_LEASE_LOST_PRESERVE_STATE")
	}
	stopped := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			wait := time.Duration(s.config.HeartbeatSeconds) * time.Second
			if remaining := time.Until(expires); remaining < wait {
				wait = remaining
			}
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			next, e := s.checkHeartbeat(ctx, c, job, expires)
			if ctx.Err() != nil {
				return
			}
			if e != nil {
				stopped <- struct{}{}
				cancel()
				return
			}
			expires = next
		}
	}()
	if err = s.checkWorksetProject(ctx, c, path, inspection.Action); err != nil {
		cancel()
		<-done
		return unknown("ASSISTANT_PROJECT_BINDING_REJECTED")
	}
	opts := Options{Workspace: s.workspace, ApprovalDigest: inspection.ApprovalDigest}
	if inspection.NetworkRequired {
		opts.SessionFile = s.config.SessionFile
		opts.CAFile = s.config.CAFile
	}
	out, actionErr := RunFile(ctx, path, opts)
	cancel()
	<-done
	if out.Schema == "" {
		out = unknown("ASSISTANT_LOCAL_ACTION_FAILED")
	}
	select {
	case <-stopped:
		out.Status = "unknown"
		out.ExitCode = 3
		out.ErrorCode = "ASSISTANT_LEASE_LOST_PRESERVE_STATE"
	default:
		if actionErr != nil && parent.Err() != nil && out.Status != "unknown" {
			out.Status = "cancelled"
			out.ExitCode = 130
			out.ErrorCode = "ASSISTANT_CANCELLED_PRESERVE_STATE"
		}
	}
	return out
}
