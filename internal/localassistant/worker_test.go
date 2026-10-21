package localassistant

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/client"
	ae "github.com/oujinhaoai/lantai/internal/contract/agentexec"
	wire "github.com/oujinhaoai/lantai/internal/contract/device"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

type workerPeer struct {
	t                           *testing.T
	mu                          sync.Mutex
	server                      *httptest.Server
	device                      wire.Device
	job                         wire.JobView
	instance, project           string
	claimCode                   string
	heartbeats, results, claims int
	dropResult                  bool
	stopHeartbeat               bool
	blockTransfer               bool
	expireOnTransfer            bool
	expireHeartbeat             bool
	upload                      *importTLSFixture
	history                     map[ids.ID]wire.JobView
	lateResult                  bool
}

func newWorkerPeer(t *testing.T) *workerPeer {
	f := &workerPeer{t: t, instance: string(ids.New()), project: "01ARZ3NDEKTSV4RRFFQ69G5FAV", claimCode: "synthetic-one-time-claim", history: map[ids.ID]wire.JobView{}}
	f.upload = &importTLSFixture{t: t}
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	f.server.Config.ErrorLog = log.New(io.Discard, "", 0)
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	return f
}
func (f *workerPeer) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/xfer/") {
		f.mu.Lock()
		blocked := f.blockTransfer
		if blocked {
			if f.expireOnTransfer {
				f.expireHeartbeat = true
			} else {
				f.stopHeartbeat = true
			}
		}
		f.mu.Unlock()
		if blocked {
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
			}
			return
		}
	}
	if strings.HasPrefix(r.URL.Path, "/api/v1/uploads") || strings.HasPrefix(r.URL.Path, "/api/v1/operations") || strings.HasPrefix(r.URL.Path, "/xfer/") {
		f.upload.serve(w, r)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	write := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }
	if r.Header.Get("Authorization") != "Bearer synthetic-import-session" {
		f.t.Error("worker used unselected credentials")
		w.WriteHeader(401)
		return
	}
	if r.URL.Path == "/api/v1/meta" {
		write(map[string]string{"api_version": "v1", "instance_id": f.instance})
		return
	}
	if r.Method == "GET" && strings.Contains(r.URL.Path, "/jobs/") {
		parts := strings.Split(r.URL.Path, "/")
		id := ids.ID(parts[len(parts)-1])
		if id == f.job.ID {
			write(f.job)
		} else if job, ok := f.history[id]; ok {
			write(job)
		} else {
			w.WriteHeader(404)
		}
		return
	}
	if r.Header.Get("Idempotency-Key") == "" {
		f.t.Error("control write omitted stable key")
	}
	switch {
	case r.URL.Path == "/api/v1/devices/claims":
		var in wire.Claim
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			f.t.Error("claim invalid")
		}
		f.claims++
		f.device = wire.Device{ID: ids.New(), ProjectID: in.ProjectID, Alias: in.Alias, WorkspaceAlias: in.WorkspaceAlias, WorkspaceBinding: in.WorkspaceBinding, Actions: in.Actions, State: "pending", Revision: 1, ExpiresAt: time.Now().Add(10 * time.Minute)}
		write(wire.ClaimResponse{Device: f.device, ClaimCode: f.claimCode})
	case strings.HasSuffix(r.URL.Path, "/poll"):
		if f.job.State == "ready" {
			f.job.State = "running"
			f.job.Fence = &ae.JobFence{JobID: f.job.ID, JobAttemptID: ids.New(), LeaseFence: 1, RecoveryEpoch: 1}
			f.job.LeaseExpiresAt = time.Now().Add(5 * time.Second).Format(time.RFC3339Nano)
		}
		var j *wire.JobView
		if f.job.ID.Valid() {
			copy := f.job
			j = &copy
		}
		write(wire.Poll{DeviceID: f.device.ID, Job: j})
	case strings.HasSuffix(r.URL.Path, "/inspect"):
		var in wire.InspectRequest
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			f.t.Error("inspect invalid")
		}
		if in.Inspection.JobDigest != f.job.JobDigest || in.Inspection.WorkspaceBinding != f.device.WorkspaceBinding {
			f.t.Error("inspect binding mismatch")
		}
		f.job.ApprovalDigest = in.Inspection.ApprovalDigest
		f.job.Inspection = &in.Inspection
		f.job.State = "awaiting_approval"
		write(f.job)
	case strings.HasSuffix(r.URL.Path, "/heartbeat"):
		f.heartbeats++
		var in wire.LeaseRequest
		_ = json.NewDecoder(r.Body).Decode(&in)
		if f.job.Fence == nil || in.Fence != *f.job.Fence {
			f.t.Error("heartbeat fence changed")
		}
		f.job.LeaseExpiresAt = time.Now().Add(5 * time.Second).Format(time.RFC3339Nano)
		if f.expireHeartbeat {
			f.job.LeaseExpiresAt = time.Now().Add(30 * time.Millisecond).Format(time.RFC3339Nano)
		}
		write(wire.Poll{DeviceID: f.device.ID, Job: &f.job, Stop: f.stopHeartbeat})
	case strings.HasSuffix(r.URL.Path, "/result"):
		var in wire.ResultRequest
		if json.NewDecoder(r.Body).Decode(&in) != nil || !in.Stopped || f.job.Fence == nil || in.Fence != *f.job.Fence {
			f.t.Error("invalid result binding")
		}
		if strings.Contains(string(in.Result), "source_root") || strings.Contains(string(in.Result), "synthetic-import-session") || strings.Contains(string(in.Result), "/xfer/") {
			f.t.Error("private execution material escaped result channel")
		}
		f.results++
		f.job.Result = in.Result
		f.job.Stopped = in.Stopped
		f.job.State = "succeeded"
		var result Result
		_ = json.Unmarshal(in.Result, &result)
		if result.Status == "unknown" || result.Status == "partial" || f.lateResult {
			f.job.State = "needs_reconciliation"
		}
		if f.dropResult {
			f.dropResult = false
			conn, _, e := w.(http.Hijacker).Hijack()
			if e != nil {
				f.t.Error(e)
			} else {
				_ = conn.Close()
			}
			return
		}
		write(f.job)
	default:
		f.t.Errorf("unexpected device path %s", r.URL.Path)
		w.WriteHeader(404)
	}
}
func (f *workerPeer) config(t *testing.T, root string) string {
	t.Helper()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.server.Certificate().Raw})
	if e := client.WritePrivate(filepath.Join(root, "credentials/ca.pem"), ca); e != nil {
		t.Fatal(e)
	}
	if e := client.SaveState(filepath.Join(root, "credentials/session.json"), map[string]string{"schema": "lantai.client-session/v1", "origin": f.server.URL, "token": "synthetic-import-session"}); e != nil {
		t.Fatal(e)
	}
	cfg := WorkerConfig{Schema: WorkerConfigSchema, Origin: f.server.URL, InstanceID: f.instance, ProjectID: f.project, Alias: "synthetic-device", WorkspaceAlias: "selected", Roots: WorkerRoots{Directory: "sources", State: "state", Destination: "working", Output: "reports"}, Actions: []string{"import.plan", "import.apply", "import.verify", "workset.plan", "workset.pull", "references.plan", "references.copy", "references.rollback"}, SessionFile: "credentials/session.json", CAFile: "credentials/ca.pem", PollSeconds: 1, HeartbeatSeconds: 1}
	if e := client.SaveState(filepath.Join(root, "requests/worker.json"), cfg); e != nil {
		t.Fatal(e)
	}
	return "requests/worker.json"
}
func (f *workerPeer) dispatch(t *testing.T, job Job) {
	t.Helper()
	raw := raw(t, job)
	canonical, e := canonjsonJob(raw)
	if e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.job.ID.Valid() {
		f.history[f.job.ID] = f.job
	}
	f.job = wire.JobView{ID: ids.New(), OperationID: ids.New(), ProjectID: ids.ID(f.project), State: "queued", Revision: 1, JobData: wire.JobData{DeviceID: f.device.ID, WorkspaceAlias: f.device.WorkspaceAlias, WorkspaceBinding: f.device.WorkspaceBinding, Job: raw, JobDigest: hash(canonical)}}
}
func (f *workerPeer) approve() { f.mu.Lock(); defer f.mu.Unlock(); f.job.State = "ready" }
func workerStep(t *testing.T, config, root string) WorkerObservation {
	t.Helper()
	o, e := WorkDevice(t.Context(), config, root, true)
	if e != nil {
		t.Fatalf("worker=%+v err=%v", o, e)
	}
	return o
}

func TestAssistantWorkerHTTPSInspectionApprovalRealPlanAndPrivateReferenceApply(t *testing.T) {
	root := workspace(t)
	f := newWorkerPeer(t)
	config := f.config(t, root)
	job := importJob(t, root)
	claim, e := EnrollDevice(t.Context(), config, root)
	if e != nil || claim.ClaimCode != f.claimCode {
		t.Fatalf("claim=%+v %v", claim, e)
	}
	saved, _ := os.ReadFile(filepath.Join(root, workerPrivateDirectory, "device.json"))
	if strings.Contains(string(saved), f.claimCode) || strings.Contains(string(saved), "synthetic-import-session") {
		t.Fatal("claim code/session persisted in device link")
	}
	f.dispatch(t, job)
	o := workerStep(t, config, root)
	if o.State != "awaiting_approval" {
		t.Fatalf("queued did not inspect %+v", o)
	}
	if _, e = os.Stat(filepath.Join(root, job.Output)); !os.IsNotExist(e) {
		t.Fatal("job executed before explicit web approval")
	}
	f.approve()
	o = workerStep(t, config, root)
	if o.State != "completed" || o.LocalResultRef == nil {
		t.Fatalf("plan did not actually complete: %+v", o)
	}
	a, e := readArtifact(root, *o.LocalResultRef)
	if e != nil {
		t.Fatal(e)
	}
	var plan client.ImportPlan
	if json.Unmarshal(a.Result.Result, &plan) != nil || plan.SelectedFiles != 1 || plan.SourceRoot != filepath.Join(root, "sources") {
		t.Fatal("real original planner was not used")
	}
	apply := Job{Schema: JobSchema, JobID: "apply-original-plan", Action: "import.apply", Origin: f.server.URL, Input: raw(t, o.LocalResultRef), Directory: "sources", State: "state/original", Output: "reports/apply.json"}
	f.dispatch(t, apply)
	workerStep(t, config, root)
	f.approve()
	o = workerStep(t, config, root)
	if o.State != "completed" || o.LocalResultRef == nil {
		t.Fatalf("real apply failed %+v", o)
	}
	f.upload.mu.Lock()
	creates, versions, data := f.upload.createCount, f.upload.versionCount, string(f.upload.bytes)
	f.upload.mu.Unlock()
	if creates != 1 || versions != 1 || data != "synthetic selected material" {
		t.Fatalf("not actual original transfer: create=%d versions=%d bytes=%q", creates, versions, data)
	}
	f.mu.Lock()
	beats := f.heartbeats
	f.mu.Unlock()
	if beats < 2 {
		t.Fatal("lease was not checked before both actions")
	}
}
func TestAssistantWorkerRestartRetriesOnlyOriginalPendingResultNotAction(t *testing.T) {
	root := workspace(t)
	f := newWorkerPeer(t)
	config := f.config(t, root)
	if _, e := EnrollDevice(t.Context(), config, root); e != nil {
		t.Fatal(e)
	}
	f.dispatch(t, importJob(t, root))
	workerStep(t, config, root)
	f.approve()
	f.mu.Lock()
	f.dropResult = true
	f.mu.Unlock()
	o, e := WorkDevice(t.Context(), config, root, true)
	if e == nil || o.State != "unknown" {
		t.Fatalf("lost result falsely accepted %+v %v", o, e)
	}
	before, e := os.ReadFile(filepath.Join(root, "reports/import-plan.json"))
	if e != nil {
		t.Fatal(e)
	}
	o = workerStep(t, config, root)
	after, _ := os.ReadFile(filepath.Join(root, "reports/import-plan.json"))
	if o.State != "completed" || string(before) != string(after) {
		t.Fatal("restart executed action rather than replaying saved original result")
	}
	f.mu.Lock()
	results, beats := f.results, f.heartbeats
	f.mu.Unlock()
	if results != 2 || beats != 1 {
		t.Fatalf("restart replay lost original identity results=%d beats=%d", results, beats)
	}
}
func TestAssistantWorkerRevokedFenceStopsBeforeActionAndCrashDoesNotRerun(t *testing.T) {
	for _, mode := range []string{"revoked", "crashed"} {
		t.Run(mode, func(t *testing.T) {
			root := workspace(t)
			f := newWorkerPeer(t)
			config := f.config(t, root)
			if _, e := EnrollDevice(t.Context(), config, root); e != nil {
				t.Fatal(e)
			}
			job := importJob(t, root)
			f.dispatch(t, job)
			workerStep(t, config, root)
			f.approve()
			if mode == "revoked" {
				f.mu.Lock()
				f.stopHeartbeat = true
				f.mu.Unlock()
			} else {
				s, e := openWorker(config, root)
				if e != nil {
					t.Fatal(e)
				}
				link, e := s.loadLink()
				if e != nil {
					t.Fatal(e)
				}
				f.mu.Lock()
				f.job.State = "running"
				f.job.Fence = &ae.JobFence{JobID: f.job.ID, JobAttemptID: ids.New(), LeaseFence: 1, RecoveryEpoch: 1}
				j := f.job
				f.mu.Unlock()
				link.Current = &j
				if e = s.save("device.json", link); e != nil {
					t.Fatal(e)
				}
				s.close()
			}
			o := workerStep(t, config, root)
			if o.State != "unknown" {
				t.Fatalf("lost/crashed lease claimed success %+v", o)
			}
			if _, e := os.Stat(filepath.Join(root, job.Output)); !os.IsNotExist(e) {
				t.Fatal("unapproved/crashed action executed")
			}
		})
	}
}
func TestAssistantWorkerPolicyAndWorkspaceLockFailClosed(t *testing.T) {
	root := workspace(t)
	f := newWorkerPeer(t)
	config := f.config(t, root)
	if _, e := EnrollDevice(t.Context(), config, root); e != nil {
		t.Fatal(e)
	}
	_, release, e := lockWorkspace(t.Context(), root)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = WorkDevice(t.Context(), config, root, true); e == nil {
		t.Fatal("second workspace worker entered")
	}
	release()
	job := importJob(t, root)
	job.Directory = "credentials"
	f.dispatch(t, job)
	if _, e = WorkDevice(t.Context(), config, root, true); e == nil {
		t.Fatal("remote alias escaped to credentials")
	}
	job = importJob(t, root)
	job.Input = raw(t, map[string]any{"schema": client.ImportInputSchema, "assets": []any{map[string]any{"key": "x", "directory": ".", "classification": "final", "push": map[string]any{"project_id": string(ids.New()), "slug": "x", "content": map[string]any{"asset_type": "doc", "files": []any{map[string]any{"path": "selected.txt", "role": "primary"}}}}}}})
	f.dispatch(t, job)
	if _, e = WorkDevice(t.Context(), config, root, true); e == nil {
		t.Fatal("remote job escaped configured project")
	}
}
func TestAssistantWorkerConfigureIsPrivateAtomicAndNeverOverwrites(t *testing.T) {
	root := workspace(t)
	f := newWorkerPeer(t)
	config := f.config(t, root)
	if _, e := ConfigureWorker(context.Background(), config, "local/worker.json", root); e != nil {
		t.Fatal(e)
	}
	if _, e := ConfigureWorker(context.Background(), config, "local/worker.json", root); e == nil {
		t.Fatal("worker original config overwritten")
	}
	if _, e := client.ReadPrivate(filepath.Join(root, "local/worker.json")); e != nil {
		t.Fatal(e)
	}
}

func TestAssistantWorkerHeartbeatRevokesActiveUploadAndHumanResumeKeepsOriginalKeys(t *testing.T) {
	for _, mode := range []string{"revocation", "expiry"} {
		t.Run(mode, func(t *testing.T) {
			root := workspace(t)
			f := newWorkerPeer(t)
			config := f.config(t, root)
			if _, e := EnrollDevice(t.Context(), config, root); e != nil {
				t.Fatal(e)
			}
			f.dispatch(t, importJob(t, root))
			workerStep(t, config, root)
			f.approve()
			plan := workerStep(t, config, root)
			apply := Job{Schema: JobSchema, JobID: "resume-original-keys", Action: "import.apply", Origin: f.server.URL, Input: raw(t, plan.LocalResultRef), Directory: "sources", State: "state/original", Output: "reports/apply.json"}
			f.dispatch(t, apply)
			workerStep(t, config, root)
			f.approve()
			f.mu.Lock()
			f.blockTransfer = true
			f.expireOnTransfer = mode == "expiry"
			f.mu.Unlock()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			o, e := WorkDevice(ctx, config, root, true)
			if e != nil || o.State != "unknown" || o.Code != "ASSISTANT_LEASE_LOST_PRESERVE_STATE" {
				t.Fatalf("active revoked upload result %+v %v", o, e)
			}
			var before client.PushState
			if e = client.LoadState(filepath.Join(root, "state/original/selected.push.json"), &before); e != nil {
				t.Fatal(e)
			}
			f.mu.Lock()
			f.blockTransfer = false
			f.stopHeartbeat = false
			f.expireHeartbeat = false
			// The synthetic controller models an explicit human reconciliation;
			// no worker-local marker can release an unresolved core attempt.
			f.job.State = "cancelled"
			f.mu.Unlock()
			// A fresh explicitly approved server job reuses the same local job/input/state;
			// neither the worker nor the original importer invents replacement keys.
			f.dispatch(t, apply)
			workerStep(t, config, root)
			f.approve()
			o = workerStep(t, config, root)
			if o.State != "completed" {
				t.Fatalf("explicit original-state resume failed %+v", o)
			}
			var after client.PushState
			if e = client.LoadState(filepath.Join(root, "state/original/selected.push.json"), &after); e != nil {
				t.Fatal(e)
			}
			if before.CreateKey != after.CreateKey || before.CommitKey != after.CommitKey || before.UploadID != after.UploadID || before.OperationID != after.OperationID {
				t.Fatal("heartbeat stop changed original keys or operation")
			}
			f.upload.mu.Lock()
			defer f.upload.mu.Unlock()
			if f.upload.createCount != 1 || f.upload.versionCount != 1 {
				t.Fatalf("resume created replacement upload/version: %d/%d", f.upload.createCount, f.upload.versionCount)
			}
		})
	}
}

func TestAssistantWorkerLocalResultReviewIsReadOnlyAndExact(t *testing.T) {
	root := workspace(t)
	f := newWorkerPeer(t)
	config := f.config(t, root)
	if _, e := EnrollDevice(t.Context(), config, root); e != nil {
		t.Fatal(e)
	}
	f.dispatch(t, importJob(t, root))
	workerStep(t, config, root)
	f.approve()
	o := workerStep(t, config, root)
	artifactPath := filepath.Join(root, workerPrivateDirectory, artifactName(o.LocalResultRef.JobID))
	before, e := os.ReadFile(artifactPath)
	if e != nil {
		t.Fatal(e)
	}
	review, e := InspectLocalResult(root, o.LocalResultRef.JobID, o.LocalResultRef.ResultSHA256)
	if e != nil || review.Result.Action != "import.plan" || hash(review.Result.Result) != o.LocalResultRef.ResultSHA256 || !strings.Contains(string(review.Result.Result), root) {
		t.Fatalf("local original plan not available for human review %+v %v", review, e)
	}
	after, _ := os.ReadFile(artifactPath)
	if string(before) != string(after) {
		t.Fatal("read-only result review modified artifact")
	}
	if _, e = InspectLocalResult(root, o.LocalResultRef.JobID, strings.Repeat("0", 64)); e == nil {
		t.Fatal("result review accepted changed original digest")
	}
	if _, e = InspectLocalResult(root, "../outside", o.LocalResultRef.ResultSHA256); e == nil {
		t.Fatal("result review accepted arbitrary path")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claims != 1 || f.results != 1 || f.heartbeats != 1 {
		t.Fatal("read-only result review made network calls")
	}
}

func TestAssistantWorkerLinuxAliasCaseAndLateCoreAcceptanceRemainUnknown(t *testing.T) {
	t.Run("case-sensitive-alias", func(t *testing.T) {
		root := workspace(t)
		f := newWorkerPeer(t)
		config := f.config(t, root)
		if e := os.Mkdir(filepath.Join(root, "SOURCES"), 0700); e != nil {
			t.Fatal(e)
		}
		a, _ := os.Stat(filepath.Join(root, "sources"))
		b, _ := os.Stat(filepath.Join(root, "SOURCES"))
		if os.SameFile(a, b) {
			t.Skip("requires a case-sensitive filesystem")
		}
		if e := os.WriteFile(filepath.Join(root, "SOURCES/selected.txt"), []byte("outside configured alias"), 0600); e != nil {
			t.Fatal(e)
		}
		if _, e := EnrollDevice(t.Context(), config, root); e != nil {
			t.Fatal(e)
		}
		job := importJob(t, root)
		job.Directory = "SOURCES"
		f.dispatch(t, job)
		o, e := WorkDevice(t.Context(), config, root, true)
		if e == nil || o.State != "blocked" {
			t.Fatalf("case sibling escaped alias: %+v %v", o, e)
		}
		if _, e = os.Stat(filepath.Join(root, job.Output)); !os.IsNotExist(e) {
			t.Fatal("outside-alias source was read by action")
		}
	})
	t.Run("late-core-result", func(t *testing.T) {
		root := workspace(t)
		f := newWorkerPeer(t)
		config := f.config(t, root)
		if _, e := EnrollDevice(t.Context(), config, root); e != nil {
			t.Fatal(e)
		}
		f.dispatch(t, importJob(t, root))
		workerStep(t, config, root)
		f.approve()
		f.mu.Lock()
		f.lateResult = true
		f.mu.Unlock()
		o := workerStep(t, config, root)
		if o.State != "unknown" || o.Code != "ASSISTANT_CORE_RECONCILIATION_REQUIRED" {
			t.Fatalf("late core unknown became local completed: %+v", o)
		}
		s, e := openWorker(config, root)
		if e != nil {
			t.Fatal(e)
		}
		link, e := s.loadLink()
		s.close()
		if e != nil || link.Unresolved == nil {
			t.Fatal("core unknown association was discarded")
		}
		next := workerStep(t, config, root)
		if next.State != "unknown" {
			t.Fatal("unknown auto resumed")
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.results != 1 || f.heartbeats != 1 {
			t.Fatal("unknown caused action/result replay rather than authoritative reconciliation")
		}
	})
}
func TestAssistantWorkerAcceptedCommitThenParentCancelPreservesUnknownKeys(t *testing.T) {
	root := workspace(t)
	f := newWorkerPeer(t)
	config := f.config(t, root)
	if _, e := EnrollDevice(t.Context(), config, root); e != nil {
		t.Fatal(e)
	}
	f.dispatch(t, importJob(t, root))
	workerStep(t, config, root)
	f.approve()
	plan := workerStep(t, config, root)
	apply := Job{Schema: JobSchema, JobID: "accepted-commit-cancel", Action: "import.apply", Origin: f.server.URL, Input: raw(t, plan.LocalResultRef), Directory: "sources", State: "state/original", Output: "reports/apply.json"}
	f.dispatch(t, apply)
	workerStep(t, config, root)
	f.approve()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.upload.mu.Lock()
	f.upload.afterCommit = cancel
	f.upload.mu.Unlock()
	o, e := WorkDevice(ctx, config, root, true)
	if e != nil || o.State != "unknown" {
		t.Fatalf("accepted commit cancellation became terminal: %+v %v", o, e)
	}
	var state client.PushState
	if e = client.LoadState(filepath.Join(root, "state/original/selected.push.json"), &state); e != nil {
		t.Fatal(e)
	}
	if !state.Committing || state.CreateKey == "" || state.CommitKey == "" || state.OperationID == "" {
		t.Fatal("cancelled accepted commit lost original unknown state")
	}
	f.upload.mu.Lock()
	defer f.upload.mu.Unlock()
	if f.upload.createCount != 1 || f.upload.versionCount != 1 || f.upload.commitKey != state.CommitKey {
		t.Fatal("fixture did not actually accept original commit before cancellation")
	}
}
