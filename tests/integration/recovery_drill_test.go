package integration

import (
	"bytes"
	"context"
	"encoding/base32"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	ax "github.com/oujinhaoai/lantai/internal/agent_execution"
	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/cli"
	"github.com/oujinhaoai/lantai/internal/client"
	ae "github.com/oujinhaoai/lantai/internal/contract/agentexec"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/masterkey"
	"github.com/oujinhaoai/lantai/internal/identity/totp"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/storage"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
	"github.com/oujinhaoai/lantai/internal/tasks"
)

// This is a synthetic archive, not evidence that any real deployment backup
// contains Task/Run/Job history. The source stays open throughout the drill.
func TestRecoveryDrillIsolatedHTTPAndSyntheticContinuation(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("isolated recovery drill is unsupported on this platform; no isolation acceptance claimed")
	}
	ctx := t.Context()
	e := newEnv(t, storage.Config{MinFreeBytes: 1 << 20}, transfer.Limits{})
	for _, role := range []identity.Role{identity.RoleOwner, identity.RoleContributor, identity.RoleViewer} {
		e.setRole(e.admin.Context.PrincipalID, role, true)
	}
	principal, oldSession := e.agent("drill-source@synthetic", identity.RoleViewer)
	principal, err := e.id.GetPrincipal(ctx, e.login().Context, principal.ID)
	if err != nil {
		t.Fatal(err)
	}
	credential := e.sudo(&identity.IssueCredential{PrincipalID: principal.ID, ExpectedRevision: principal.Revision, Scopes: []identity.Scope{identity.ScopeRead}})
	body := []byte("synthetic fixed version from recovery archive")
	version := e.ingest(e.login().Context, "drill/fixed", body, *rightsOwned())
	// Preserve a real permission negative in the archive: administrator status
	// alone must not grant project content access.
	visibleProject := e.project
	hiddenProject, err := e.catalog.CreateProject(ctx, catalog.ProjectRequest{Who: e.login().Context, IdempotencyKey: e.key(), Key: "drill-private", Name: "Synthetic private project"})
	if err != nil {
		t.Fatal(err)
	}
	e.project = hiddenProject
	e.setRole(e.admin.Context.PrincipalID, identity.RoleContributor, true)
	hiddenVersion := e.ingest(e.login().Context, "drill/private", []byte("synthetic private content"), *rightsOwned())
	e.setRole(e.admin.Context.PrincipalID, identity.RoleContributor, false)
	e.project = visibleProject
	source := reopenApplication(t, e)
	oldKey, err := masterkey.Load(source.Instance.Config().SecretsDir)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "archive")
	backup, err := source.Backup(ctx, operations.BackupOptions{Destination: archive})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(parent, "clone")
	marker, err := application.Restore(ctx, operations.RestoreOptions{Home: home, BackupDir: archive}, oldKey, identity.Config{Password: fastPassword})
	if err != nil {
		t.Fatal(err)
	}
	opts := application.Options{Instance: operations.Options{Home: home, Clock: e.clk}, Identity: identity.Config{Password: fastPassword}, Storage: storage.Config{MinFreeBytes: 1 << 20}}
	drillOpts := opts
	drillOpts.Instance.RecoveryDrill = true
	if a, err := application.Open(ctx, drillOpts); err == nil {
		a.Close(context.Background())
		t.Fatal("drill accepted without recovered administrator proof")
	}
	maintenanceOpts := opts
	maintenanceOpts.Instance.DrillMaintenance = true
	offline, err := application.OpenOffline(ctx, maintenanceOpts)
	if err != nil {
		t.Fatal(err)
	}
	const password = "synthetic recovered administrator password"
	var secret []byte
	err = offline.Maintenance(ctx, func(c context.Context) error {
		reset, err := offline.Identity.BeginOfflineReset(c, "ada", password)
		if err != nil {
			return err
		}
		secret, err = base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(reset.Enrollment.Secret)
		if err != nil {
			return err
		}
		_, err = reset.Confirm(c, totp.Code(secret, totp.Counter(e.clk.Now())), "synthetic isolated recovery test")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = offline.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	drill, err := application.Open(ctx, drillOpts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { drill.Close(context.Background()) })
	if drill.Instance.Marker().Restore == nil || drill.Instance.Marker().Restore.Stage != "reconciliation_required" {
		t.Fatal("drill removed formal recovery latch")
	}
	if !source.Ready() {
		t.Fatal("source service lost readiness during independent clone drill")
	}
	if drill.Jobs != nil || drill.Flows != nil || drill.ExtensionManager != nil || drill.Scheduler != nil {
		t.Fatal("drill wired external execution or scheduler services")
	}
	if _, err = drill.Identity.VerifySession(ctx, oldSession.Context.SessionID); err == nil {
		t.Fatal("archived session survived")
	}
	if _, err = drill.Identity.ExchangeToken(ctx, credential.Secret, identity.SessionRequest{Channel: identity.ChannelCLI}); err == nil {
		t.Fatal("archived credential survived")
	}
	e.clk.Advance(time.Duration(totp.Period) * time.Second)
	if _, err = drill.Identity.Login(ctx, identity.LoginRequest{Name: "ada", Password: adminPassword, Code: totp.Code(e.secret, totp.Counter(e.clk.Now())), Channel: identity.ChannelCLI, Source: "127.0.0.1"}); err == nil {
		t.Fatal("old administrator credentials survived")
	}
	login, err := drill.Identity.Login(ctx, identity.LoginRequest{Name: "ada", Password: password, Code: totp.Code(secret, totp.Counter(e.clk.Now())), Channel: identity.ChannelCLI, Source: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := drill.Instance.Config()
	cfg.Listen = operations.ListenConfig{API: "127.0.0.1:0", Transfer: "127.0.0.1:0", Operations: "127.0.0.1:0"}
	for _, field := range []string{"api", "transfer", "operations"} {
		for _, address := range []string{"0.0.0.0:0", "localhost:0", "[::]:0"} {
			bad := cfg
			switch field {
			case "api":
				bad.Listen.API = address
			case "transfer":
				bad.Listen.Transfer = address
			case "operations":
				bad.Listen.Operations = address
			}
			if s, err := drill.StartHTTP(ctx, bad); err == nil {
				s.Shutdown(context.Background())
				t.Fatalf("unsafe %s address accepted: %s", field, address)
			}
		}
	}
	servers, err := drill.StartHTTP(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { servers.Shutdown(context.Background()) })
	request := func(method, path, token, host, header string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, "http://"+servers.Addresses.API+path, bytes.NewBufferString("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if host != "" {
			req.Host = host
		}
		if header != "" {
			req.Header.Set(header, "127.0.0.1")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, b
	}
	sessionFile := filepath.Join(t.TempDir(), "drill-session.json")
	writeJSON(t, sessionFile, map[string]string{"schema": "lantai.client-session/v1", "origin": "http://" + servers.Addresses.API, "token": login.Token})
	var stdout, stderr bytes.Buffer
	code := cli.Run(ctx, []string{"show", "--ref", version.URI, "--view", "full", "--server", "http://" + servers.Addresses.API, "--allow-http", "--session-file", sessionFile}, &stdout, &stderr)
	if code != 0 || !bytes.Contains(stdout.Bytes(), []byte(version.VersionID)) {
		t.Fatalf("official CLI fixed reference read: %d %s", code, stderr.String())
	}
	fixedPath := "/api/v1/assets/" + string(version.AssetID) + "/versions/" + string(version.VersionID)
	if status, b := request("GET", fixedPath, login.Token, "", ""); status != 200 {
		t.Fatalf("fixed version read: %d %s", status, b)
	}
	for _, token := range []string{"", oldSession.Token} {
		if status, _ := request("GET", fixedPath, token, "", ""); status == 200 {
			t.Fatal("read without fresh auth")
		}
	}
	for _, host := range []string{"attacker.example", "localhost", "127.0.0.1.attacker.example"} {
		if status, _ := request("GET", fixedPath, login.Token, host, ""); status != http.StatusForbidden {
			t.Fatalf("unsafe Host accepted: %s", host)
		}
	}
	for _, header := range []string{"Forwarded", "X-Forwarded-Host", "X-Forwarded-For", "X-Forwarded-Proto"} {
		if status, _ := request("GET", fixedPath, login.Token, "", header); status != http.StatusForbidden {
			t.Fatalf("proxy header accepted: %s", header)
		}
	}
	for _, path := range []string{"/api/v1/jobs", "/api/v1/flows", "/api/v1/projects", "/api/v1/human/prepare", "/api/v1/human/execute", "/api/v1/backups", "/api/v1/extensions", "/api/v1/nodes", "/api/v1/ingest"} {
		if status, b := request("POST", path, login.Token, "", ""); status != http.StatusForbidden {
			t.Fatalf("side effect route accepted: %s %d %s", path, status, b)
		}
	}
	grant, err := drill.Storage.IssueReadGrant(ctx, storage.ReadRequest{Who: login.Context, AssetID: version.AssetID, VersionID: version.VersionID, Path: "content.txt", Purpose: "archive_review"})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+servers.Addresses.Transfer+grant.URL, nil)
	req.Header.Set("Authorization", "Bearer "+login.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || !bytes.Equal(got, body) {
		t.Fatalf("fixed version bytes: %d %q %v", resp.StatusCode, got, err)
	}
	hiddenPath := "/api/v1/assets/" + string(hiddenVersion.AssetID) + "/versions/" + string(hiddenVersion.VersionID)
	if status, _ := request("GET", hiddenPath, login.Token, "", ""); status != http.StatusNotFound && status != http.StatusForbidden {
		t.Fatalf("drill bypassed project read permissions: %d", status)
	}
	run, in := syntheticDrillContinuation(t, e, drill, servers, login)
	if err = servers.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err = drill.Close(ctx); err != nil {
		t.Fatal(err)
	}
	drill, err = application.OpenRecoveryDrill(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	servers = remoteServers(t, drill, true)
	c, err := client.New(client.Config{BaseURL: "http://" + servers.Addresses.API, SessionToken: login.Token, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	read, err := c.Do(ctx, "GET", "/api/v1/task-runs/"+string(run.Run.ID), nil, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var resumed ax.Record
	if err = read.Decode(&resumed); err != nil || resumed.Run.ID != run.Run.ID || resumed.Usage.ModelCalls != 1 {
		t.Fatalf("drill restart lost run: %+v %v", resumed, err)
	}
	progress := ax.ProgressRequest{Mutation: mut(resumed, in.Fence), Usage: ae.BudgetUsage{ModelCalls: 2}}
	key := e.key()
	for attempt := 0; attempt < 2; attempt++ {
		result, err := c.Do(ctx, "POST", "/api/v1/task-runs/progress", progress, client.Options{IdempotencyKey: key})
		if err != nil {
			t.Fatalf("post-restart progress/replay: %v", err)
		}
		var updated ax.Record
		if err = result.Decode(&updated); err != nil || updated.Usage.ModelCalls != 2 {
			t.Fatalf("post-restart progress: %+v %v", updated, err)
		}
		if attempt == 0 {
			resumed = updated
		} else if updated.Run.Revision != resumed.Run.Revision {
			t.Fatal("idempotent replay advanced revision")
		}
	}
	stale := ax.ProgressRequest{Mutation: mut(resumed, in.Fence), Usage: ae.BudgetUsage{ModelCalls: 3}}
	stale.Fence.LeaseFence++
	_, err = c.Do(ctx, "POST", "/api/v1/task-runs/progress", stale, client.Options{IdempotencyKey: e.key()})
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Body.Code != errcode.LeaseStale {
		t.Fatalf("expected stale fence rejection after restart, got %v", err)
	}

	// Checkpoint is a real manual stop record, not just a progress observation.
	checkpoint := ae.Checkpoint{Contract: "lantai.checkpoint/v1", ID: ids.New(), TaskRunID: resumed.Run.ID, AttemptID: resumed.Run.CurrentAttemptID, Sequence: 1, InputSnapshotDigest: resumed.Run.Input.Digest, ProfileDigest: resumed.Run.Profile.Digest, BackendVersion: "1.0.0", FormatVersion: "1", ResumeClass: execution.ResumePortableArtifacts, ArtifactRefs: []ids.PermanentRef{}, CompletedStepIDs: []ids.ID{}, CreatedAt: clock.Format(e.clk.Now())}
	saved, err := c.Do(ctx, "POST", "/api/v1/task-runs/checkpoint", ax.CheckpointRequest{Mutation: mut(resumed, in.Fence), Checkpoint: checkpoint, Stopped: true}, client.Options{IdempotencyKey: e.key()})
	if err != nil {
		t.Fatal("persist checkpoint", err)
	}
	var checkpointed ax.Record
	if err = saved.Decode(&checkpointed); err != nil || checkpointed.Run.State != execution.RunPaused || !checkpointed.Run.Termination.Confirmed {
		t.Fatalf("checkpoint did not stop manual run: %+v %v", checkpointed, err)
	}
	if err = servers.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err = drill.Close(ctx); err != nil {
		t.Fatal(err)
	}
	drill, err = application.OpenRecoveryDrill(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	servers = remoteServers(t, drill, true)
	checkpointClient, err := client.New(client.Config{BaseURL: "http://" + servers.Addresses.API, SessionToken: login.Token, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer checkpointClient.Close()
	read, err = checkpointClient.Do(ctx, "GET", "/api/v1/task-runs/"+string(resumed.Run.ID), nil, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var restoredCheckpoint ax.Record
	if err = read.Decode(&restoredCheckpoint); err != nil || restoredCheckpoint.Run.ID != resumed.Run.ID || restoredCheckpoint.Run.State != execution.RunPaused || len(restoredCheckpoint.Checkpoints) != 1 {
		t.Fatalf("restart lost checkpoint: %+v %v", restoredCheckpoint, err)
	}
	cp := restoredCheckpoint.Checkpoints[0]
	if cp.ID != checkpoint.ID || cp.TaskRunID != checkpoint.TaskRunID || cp.AttemptID != checkpoint.AttemptID || cp.Sequence != 1 || cp.InputSnapshotDigest != checkpoint.InputSnapshotDigest || cp.ProfileDigest != checkpoint.ProfileDigest || restoredCheckpoint.Usage.ModelCalls != 2 {
		t.Fatalf("checkpoint binding or cumulative usage changed: %+v", restoredCheckpoint)
	}
	// A separate still-running lease tests real elapsed expiration, not a forged
	// fence. The public session outlives this 31-minute synthetic lease advance.
	expiring, expiringInput := syntheticDrillContinuation(t, e, drill, servers, login)
	e.clk.Advance(31 * time.Minute)
	_, err = checkpointClient.Do(ctx, "POST", "/api/v1/task-runs/progress", ax.ProgressRequest{Mutation: mut(expiring, expiringInput.Fence), Usage: ae.BudgetUsage{ModelCalls: 2}}, client.Options{IdempotencyKey: e.key()})
	apiErr = nil
	if !errors.As(err, &apiErr) || apiErr.Body.Code != errcode.LeaseStale {
		t.Fatalf("expired lease was not rejected as LEASE_STALE: %v", err)
	}

	if _, err = drill.Backup(ctx, operations.BackupOptions{Destination: filepath.Join(t.TempDir(), "forbidden")}); err == nil {
		t.Fatal("drill generated backup")
	}
	evidence := []byte("synthetic test deliberately must not authorize production takeover")
	r := marker.Restore
	review := operations.RecoveryReview{Contract: "lantai.recovery-review/v1", RunID: r.RunID, BackupID: backup.BackupID, ManifestDigest: r.ManifestDigest, RecoveryEpoch: marker.RecoveryEpoch, Administrator: string(e.admin.Context.PrincipalID), RevocationsReconciled: true, DeletionsReconciled: true, OpenOperationsReconciled: true, EvidenceDigest: digest.Of(evidence), Note: "synthetic drill"}
	if err = drill.CompleteRestore(ctx, review, evidence); err == nil {
		t.Fatal("drill completed formal restore")
	}
	if err = servers.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err = drill.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if a, err := application.Open(ctx, opts); err == nil {
		a.Close(context.Background())
		t.Fatal("ordinary serve accepted previously drilled clone")
	}
	if _, err = operations.VerifyBackup(ctx, archive); err != nil {
		t.Fatal("source archive changed", err)
	}
	if !source.Ready() {
		t.Fatal("source was marked offline")
	}
}

func syntheticDrillContinuation(t *testing.T, e *env, a *application.App, s *application.HTTPServers, login identity.IssuedSession) (ax.Record, ae.StartRequest) {
	t.Helper()
	c, err := client.New(client.Config{BaseURL: "http://" + s.Addresses.API, SessionToken: login.Token, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	post := func(path string, in, out any) {
		t.Helper()
		r, err := c.Do(t.Context(), "POST", "/api/v1/"+path, in, client.Options{IdempotencyKey: e.key()})
		if err != nil {
			t.Fatal(path, err)
		}
		if out != nil {
			if err = r.Decode(out); err != nil {
				t.Fatal(err)
			}
		}
	}
	var created tasks.Result
	post("tasks", tasks.CreateRequest{ProjectID: e.project.ProjectID, Type: "question", Title: "Synthetic drill continuation, not real backup history", AcceptanceCriteria: []string{"record local manual progress"}, Role: identity.RoleContributor}, &created)
	r, err := c.Do(t.Context(), "GET", "/api/v1/tasks/"+string(created.TaskID), nil, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var v tasks.View
	if err = r.Decode(&v); err != nil {
		t.Fatal(err)
	}
	var claimed tasks.Result
	post("tasks/claim", tasks.ClaimRequest{ProjectID: e.project.ProjectID, TaskID: v.Task.ID, SeatID: v.Seat.ID, ExpectedRevision: v.Seat.Revision}, &claimed)
	raw, err := os.ReadFile("../../schemas/examples/agent-execution/v1/agent-request/start.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Document ae.StartRequest `json:"document"`
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	in := fixture.Document
	in.OperationID = ids.New()
	in.TaskRunID = ids.New()
	in.BudgetID = ids.New()
	in.Fence = claimed.Attempt.Fence
	in.Fence.TaskID = v.Task.ID
	in.Input = v.Task.Input
	in.Activation = nil
	in.Profile.ActivationSnapshot = ae.ActivationSnapshot{}
	in.Profile.PlaybookRefs = []ids.PermanentRef{}
	in.Profile.RequiredCapabilities = []string{"manual_session"}
	in.Profile.AllowedTools = []string{"catalog.read"}
	in.ExecutionKey, _ = ae.ExecutionKey(in.TaskRunID, in.Fence.AttemptID)
	in.RequestHash, _ = ae.RequestHash(in)
	var run ax.Record
	post("task-runs", in, &run)
	post("task-runs/progress", ax.ProgressRequest{Mutation: mut(run, in.Fence), Usage: ae.BudgetUsage{ModelCalls: 1}}, &run)
	r, err = c.Do(t.Context(), "GET", "/api/v1/task-runs/"+string(run.Run.ID), nil, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var persisted ax.Record
	if err = r.Decode(&persisted); err != nil || persisted.Usage.ModelCalls != 1 {
		t.Fatalf("manual progress not persisted: %+v %v", persisted, err)
	}
	return persisted, in
}
