package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	dc "github.com/oujinhaoai/lantai/internal/contract/device"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/httpauth"
	"github.com/oujinhaoai/lantai/internal/localassistant"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

type deviceWorkerCoreFixture struct {
	t            *testing.T
	env          *env
	app          *application.App
	front        *httptest.Server
	browser      identity.IssuedSession
	cliSession   identity.IssuedSession
	afterCommit  *atomic.Pointer[func()]
	device       dc.Device
	root, config string
	jobs         map[string]dc.JobView
}

func newDeviceWorkerCoreFixture(t *testing.T) *deviceWorkerCoreFixture {
	t.Helper()
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20, PartSize: 64 << 10}, transfer.Limits{})
	e.clk.Set(time.Now().UTC())
	admin := e.login()
	e.setRole(admin.Context.PrincipalID, identity.RoleContributor, true)
	scopes := []identity.Scope{identity.ScopeRead, identity.ScopeIngest, identity.ScopeOrganize}
	cliSession, err := e.id.Login(t.Context(), identity.LoginRequest{Name: "ada", Password: adminPassword, Code: e.fresh(), Source: "192.0.2.20", Channel: identity.ChannelCLI, Scopes: scopes, Projects: []ids.ID{e.project.ProjectID}, TTL: 15 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	browser, err := e.id.Login(t.Context(), identity.LoginRequest{Name: "ada", Password: adminPassword, Code: e.fresh(), Source: "192.0.2.21", Channel: identity.ChannelBrowser, Scopes: scopes, Projects: []ids.ID{e.project.ProjectID}, TTL: 15 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	// Login helpers consume successive TOTP steps. Put the fake clock near the
	// resulting session creation time so real worker expiry checks remain meaningful.
	a := reopenApplication(t, e)
	var target atomic.Pointer[url.URL]
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(target.Load()) }}
	commitHook := &atomic.Pointer[func()]{}
	proxy.ModifyResponse = func(r *http.Response) error {
		if r.Request.Method == http.MethodPost && strings.HasSuffix(r.Request.URL.Path, "/commit") && r.StatusCode >= 200 && r.StatusCode < 300 {
			if hook := commitHook.Swap(nil); hook != nil {
				(*hook)()
				r.Body = &cutReadCloser{ReadCloser: r.Body, left: 0}
			}
		}
		return nil
	}
	front := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/") && !strings.HasPrefix(r.URL.Path, "/xfer/v1/") {
			http.NotFound(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	cfg := a.Instance.Config()
	cfg.Listen = operations.ListenConfig{API: "127.0.0.1:0", Transfer: "127.0.0.1:0", Operations: "127.0.0.1:0", Merged: true}
	cfg.HTTP.AllowedOrigins = []string{front.URL}
	servers, err := a.StartHTTP(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := servers.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	coreURL, _ := url.Parse("http://" + servers.Addresses.API)
	target.Store(coreURL)
	root := assetRealTempDir(t)
	for _, dir := range []string{"requests", "credentials", "sources/material", "sources/notes"} {
		if err = os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err = client.WritePrivate(filepath.Join(root, "credentials/ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: front.Certificate().Raw})); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(root, "credentials/device-session.json"), map[string]string{"schema": "lantai.client-session/v1", "origin": front.URL, "token": cliSession.Token})
	config := localassistant.WorkerConfig{Schema: localassistant.WorkerConfigSchema, Origin: front.URL, InstanceID: string(a.Instance.InstanceID()), ProjectID: string(e.project.ProjectID), Alias: "core-synthetic", WorkspaceAlias: "selected", Roots: localassistant.WorkerRoots{Directory: "sources", State: "state", Destination: "working", Output: "reports"}, Actions: []string{"import.plan", "import.apply", "import.verify", "workset.plan", "workset.pull", "references.plan", "references.copy", "references.rollback"}, SessionFile: "credentials/device-session.json", CAFile: "credentials/ca.pem", PollSeconds: 1, HeartbeatSeconds: 1}
	writeJSON(t, filepath.Join(root, "requests/worker.json"), config)
	f := &deviceWorkerCoreFixture{t: t, env: e, app: a, front: front, browser: browser, cliSession: cliSession, afterCommit: commitHook, root: root, config: "requests/worker.json", jobs: map[string]dc.JobView{}}
	claim, err := localassistant.EnrollDevice(t.Context(), f.config, root)
	if err != nil {
		t.Fatal(err)
	}
	if claim.ClaimCode == "" {
		t.Fatal("fresh core claim omitted once-only confirmation")
	}
	f.web(http.MethodPost, "/api/v1/devices/"+string(claim.Device.ID)+"/approve", dc.ApproveDevice{ExpectedRevision: claim.Device.Revision, ClaimCode: claim.ClaimCode, WorkspaceAlias: claim.Device.WorkspaceAlias, WorkspaceBinding: claim.Device.WorkspaceBinding}, &f.device)
	return f
}
func (f *deviceWorkerCoreFixture) web(method, path string, body any, out any) {
	f.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(f.t.Context(), method, f.front.URL+path, reader)
	if err != nil {
		f.t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: httpauth.CookieName, Value: f.browser.Token})
	req.Header.Set("Origin", f.front.URL)
	if method != "GET" {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", f.browser.CSRFToken)
		req.Header.Set("Idempotency-Key", string(ids.New()))
	}
	res, err := f.front.Client().Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode < 200 || res.StatusCode >= 300 {
		f.t.Fatalf("real core %s %s: status=%d %s %v", method, path, res.StatusCode, raw, err)
	}
	if out != nil && json.Unmarshal(raw, out) != nil {
		f.t.Fatal("invalid real core reply")
	}
}
func (f *deviceWorkerCoreFixture) runJob(job localassistant.Job) localassistant.LocalResultRef {
	f.t.Helper()
	raw, err := json.Marshal(job)
	if err != nil {
		f.t.Fatal(err)
	}
	canonical, err := canonjson.Canonicalize(raw)
	if err != nil {
		f.t.Fatal(err)
	}
	base := "/api/v1/devices/" + string(f.device.ID)
	f.web(http.MethodGet, base, nil, &f.device)
	var view dc.JobView
	f.web(http.MethodPost, base+"/jobs", dc.CreateJob{ExpectedDeviceRevision: f.device.Revision, ProjectID: f.env.project.ProjectID, WorkspaceAlias: f.device.WorkspaceAlias, Job: raw, JobDigest: shaOf(canonical)}, &view)
	observation, err := localassistant.WorkDevice(f.t.Context(), f.config, f.root, true)
	if err != nil || observation.State != "awaiting_approval" {
		f.t.Fatalf("real worker inspect %+v %v", observation, err)
	}
	f.web(http.MethodGet, base+"/jobs/"+string(view.ID), nil, &view)
	if view.State != "awaiting_approval" || view.Fence != nil || view.Inspection == nil {
		f.t.Fatalf("core issued execution before human digest approval: %+v", view)
	}
	f.web(http.MethodPost, base+"/jobs/"+string(view.ID)+"/approve", dc.ApproveJob{ExpectedRevision: view.Revision, JobDigest: view.JobDigest, ApprovalDigest: view.ApprovalDigest, WorkspaceBinding: view.WorkspaceBinding}, &view)
	observation, err = localassistant.WorkDevice(f.t.Context(), f.config, f.root, true)
	if err != nil || observation.State != "completed" || observation.LocalResultRef == nil {
		f.t.Fatalf("real worker %s: %+v %v", job.Action, observation, err)
	}
	f.web(http.MethodGet, base+"/jobs/"+string(view.ID), nil, &view)
	if view.State != "succeeded" || view.Fence == nil || !view.Stopped || view.Fence.JobID != view.ID {
		f.t.Fatalf("real T06 did not accept exact action/fence: %+v", view)
	}
	if strings.Contains(string(view.Result), f.root) || strings.Contains(string(view.Result), "source_root") || strings.Contains(string(view.Result), "/xfer/") {
		f.t.Fatal("network result exposed private workspace/transfer information")
	}
	f.jobs[job.Action] = view
	return *observation.LocalResultRef
}
func (f *deviceWorkerCoreFixture) runEightActions() {
	t := f.t
	material := []byte("synthetic real-core worker material\n")
	if err := os.WriteFile(filepath.Join(f.root, "sources/material/sample.txt"), material, 0600); err != nil {
		t.Fatal(err)
	}
	input := client.ImportInput{Schema: client.ImportInputSchema, Assets: []client.ImportAsset{{Key: "sample", Directory: ".", Classification: "final", Push: client.PushInput{ProjectID: string(f.env.project.ProjectID), Slug: "synthetic/core-worker", Content: client.ContentInput{AssetType: "doc", Rights: mustJSON(rightsOwned()), Files: []client.InputFile{{Path: "sample.txt", Role: "primary"}}}, Describe: mustJSON(map[string]any{"title": "Synthetic worker material"})}}}}
	plan := f.runJob(localassistant.Job{Schema: localassistant.JobSchema, JobID: "core-import-plan", Action: "import.plan", Input: mustJSON(input), Directory: "sources/material", Output: "reports/import-plan.json"})
	applied := f.runJob(localassistant.Job{Schema: localassistant.JobSchema, JobID: "core-import-apply", Action: "import.apply", Origin: f.front.URL, Input: mustJSON(plan), Directory: "sources/material", State: "state/import", Output: "reports/import-apply.json"})
	f.runJob(localassistant.Job{Schema: localassistant.JobSchema, JobID: "core-import-verify", Action: "import.verify", Origin: f.front.URL, Input: mustJSON(plan), Report: mustJSON(applied), Directory: "sources/material", State: "state/import", Destination: "working/verification", Output: "reports/import-verify.json", Purpose: "archive_review"})
	var result localassistant.Result
	if err := client.LoadState(filepath.Join(f.root, "reports/import-apply.json"), &result); err != nil {
		t.Fatal(err)
	}
	var report client.ImportReport
	if json.Unmarshal(result.Result, &report) != nil || !report.Complete || len(report.Results) != 1 {
		t.Fatal("original import report was not actually completed")
	}
	fixed := report.Results[0]
	workset := client.WorksetInput{Schema: client.WorksetSchema, Entries: []client.WorksetEntry{{AssetID: fixed.AssetID, VersionID: fixed.VersionID, Destination: "entry"}}, Entrypoints: []string{"entry/sample.txt"}}
	f.runJob(localassistant.Job{Schema: localassistant.JobSchema, JobID: "core-workset-plan", Action: "workset.plan", Origin: f.front.URL, Input: mustJSON(workset), Directory: "sources/workset", Output: "reports/workset-plan.json"})
	f.runJob(localassistant.Job{Schema: localassistant.JobSchema, JobID: "core-workset-pull", Action: "workset.pull", Origin: f.front.URL, Input: mustJSON(workset), Directory: "sources/workset", Output: "reports/workset-pull.json", Purpose: "archive_review"})
	for _, path := range []string{"working/verification/sample/sample.txt", "sources/workset/entry/sample.txt"} {
		b, err := os.ReadFile(filepath.Join(f.root, path))
		if err != nil || !bytes.Equal(b, material) {
			t.Fatalf("actual downloaded bytes differ at %s: %v", path, err)
		}
	}
	uri := "lantai://" + string(f.app.Instance.InstanceID()) + "/assets/" + fixed.AssetID + "/versions/" + fixed.VersionID
	before := "# Synthetic note\r\n[Original](../legacy/sample.txt)\r\n"
	if err := os.WriteFile(filepath.Join(f.root, "sources/notes/note.md"), []byte(before), 0600); err != nil {
		t.Fatal(err)
	}
	refInput := client.ReferenceInput{Schema: client.ReferenceInputSchema, Notes: []string{"note.md"}, Mappings: []client.ReferenceMapping{{OldTarget: "../legacy/sample.txt", FixedRef: uri}}}
	refPlan := f.runJob(localassistant.Job{Schema: localassistant.JobSchema, JobID: "references-synthetic-device-batch-plan", Action: "references.plan", Input: mustJSON(refInput), Directory: "sources/notes", Output: "reports/reference-plan.json"})
	f.runJob(localassistant.Job{Schema: localassistant.JobSchema, JobID: "references-synthetic-device-batch-copy", Action: "references.copy", Input: mustJSON(refPlan), Directory: "sources/notes", Destination: "working/note-copy", Output: "reports/reference-copy.json"})
	f.runJob(localassistant.Job{Schema: localassistant.JobSchema, JobID: "references-synthetic-device-batch-rollback", Action: "references.rollback", Input: mustJSON(refPlan), Directory: "working/note-copy", Destination: "working/rollback", Output: "reports/reference-rollback.json", PostCutoverRefs: []string{}})
	original, _ := os.ReadFile(filepath.Join(f.root, "sources/notes/note.md"))
	copy, _ := os.ReadFile(filepath.Join(f.root, "working/note-copy/note.md"))
	rolled, _ := os.ReadFile(filepath.Join(f.root, "working/rollback/note.md"))
	if string(original) != before || !strings.Contains(string(copy), uri) || string(rolled) != before {
		t.Fatal("actual reference source/copy/rollback bytes do not match original client semantics")
	}
	if len(f.jobs) != 8 {
		t.Fatalf("expected all eight actual core jobs, got %d", len(f.jobs))
	}
}
func TestDeviceWorkerClientRealCoreHTTPSAllEightActions(t *testing.T) {
	f := newDeviceWorkerCoreFixture(t)
	f.runEightActions()
}

// The real core accepts the catalog commit first; only then the TLS gateway
// cancels the parent and withholds its response. Both client and T06 must retain
// unresolved external effects rather than acknowledge a terminal cancellation.
func TestDeviceWorkerClientRealCoreAcceptedCommitThenCancelIsUnknown(t *testing.T) {
	f := newDeviceWorkerCoreFixture(t)
	if err := os.WriteFile(filepath.Join(f.root, "sources/material/sample.txt"), []byte("synthetic accepted commit before cancellation\n"), 0600); err != nil {
		t.Fatal(err)
	}
	input := client.ImportInput{Schema: client.ImportInputSchema, Assets: []client.ImportAsset{{Key: "sample", Directory: ".", Classification: "final", Push: client.PushInput{ProjectID: string(f.env.project.ProjectID), Slug: "synthetic/cancelled-observer", Content: client.ContentInput{AssetType: "doc", Rights: mustJSON(rightsOwned()), Files: []client.InputFile{{Path: "sample.txt", Role: "primary"}}}, Describe: mustJSON(map[string]any{"title": "Synthetic accepted commit"})}}}}
	plan := f.runJob(localassistant.Job{Schema: localassistant.JobSchema, JobID: "cancel-core-plan", Action: "import.plan", Input: mustJSON(input), Directory: "sources/material", Output: "reports/cancel-plan.json"})
	job := localassistant.Job{Schema: localassistant.JobSchema, JobID: "cancel-core-apply", Action: "import.apply", Origin: f.front.URL, Input: mustJSON(plan), Directory: "sources/material", State: "state/cancel-import", Output: "reports/cancel-apply.json"}
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := canonjson.Canonicalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/devices/" + string(f.device.ID)
	f.web(http.MethodGet, base, nil, &f.device)
	var view dc.JobView
	f.web(http.MethodPost, base+"/jobs", dc.CreateJob{ExpectedDeviceRevision: f.device.Revision, ProjectID: f.env.project.ProjectID, WorkspaceAlias: f.device.WorkspaceAlias, Job: raw, JobDigest: shaOf(canonical)}, &view)
	observed, err := localassistant.WorkDevice(t.Context(), f.config, f.root, true)
	if err != nil || observed.State != "awaiting_approval" {
		t.Fatalf("inspect %+v %v", observed, err)
	}
	f.web(http.MethodGet, base+"/jobs/"+string(view.ID), nil, &view)
	f.web(http.MethodPost, base+"/jobs/"+string(view.ID)+"/approve", dc.ApproveJob{ExpectedRevision: view.Revision, JobDigest: view.JobDigest, ApprovalDigest: view.ApprovalDigest, WorkspaceBinding: view.WorkspaceBinding}, &view)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	callback := func() { cancel() }
	f.afterCommit.Store(&callback)
	observed, err = localassistant.WorkDevice(ctx, f.config, f.root, true)
	if err != nil || observed.State != "unknown" {
		t.Fatalf("accepted commit was labelled terminal: %+v %v", observed, err)
	}
	f.web(http.MethodGet, base+"/jobs/"+string(view.ID), nil, &view)
	if view.State != "needs_reconciliation" || !view.Stopped {
		t.Fatalf("T06 cleared unknown external effect: %+v", view)
	}
	var state client.PushState
	if err = client.LoadState(filepath.Join(f.root, "state/cancel-import/sample.push.json"), &state); err != nil {
		t.Fatal(err)
	}
	if !state.Committing || state.CreateKey == "" || state.CommitKey == "" || state.OperationID == "" {
		t.Fatal("original unknown committing identity was not retained")
	}
	upload, err := f.app.Storage.GetUpload(t.Context(), f.cliSession.Context, ids.ID(state.UploadID))
	if err != nil || upload.State != storage.UploadCompleted || upload.CloseReason != "committed" {
		t.Fatalf("fixture did not actually accept the catalog commit: %+v %v", upload, err)
	}
	next, err := localassistant.WorkDevice(t.Context(), f.config, f.root, true)
	if err != nil || next.State != "unknown" || next.JobID != view.ID {
		t.Fatalf("restart did not retain original core reconciliation association: %+v %v", next, err)
	}
}
